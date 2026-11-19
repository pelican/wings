package cron

import (
	"context"
	"net"

	"emperror.dev/errors"

	"github.com/pelican/wings/internal/database"
	"github.com/pelican/wings/internal/models"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/system"
)

type activityCron struct {
	mu      *system.AtomicBool
	manager *server.Manager
	max     int
	// after is the ID of the last row read. Rows the Panel refuses stay stored,
	// so reading carries on from here, across runs, rather than from the oldest
	// row. It goes back to zero once the end is reached, so refused rows are
	// still retried.
	after int
}

// Run executes the cronjob and ensures we fetch and send all the stored activity to the
// Panel instance. Once activity is sent it is deleted from the local database instance. Any
// SFTP specific events are not handled in this cron, they're handled separately to account
// for de-duplication and event merging.
func (ac *activityCron) Run(ctx context.Context) error {
	// Don't execute this cron if there is currently one running. Once this task is completed
	// go ahead and mark it as no longer running.
	if !ac.mu.SwapIf(true) {
		return errors.WithStack(ErrCronRunning)
	}
	defer ac.mu.Store(false)

	if err := pruneActivity(ctx, maxStoredActivity); err != nil {
		return err
	}

	// Keep sending batches until the stored activity has been sent, so that it is
	// sent as quickly as it is created.
	// Activity the Panel refuses stays stored, so each batch starts after the
	// last row read rather than at the oldest row. Otherwise refused activity
	// would be read again and again, and newer activity never sent, even across
	// runs once there is more refused activity than one run sends.
	for i := 0; i < maxBatchesPerRun; i++ {
		n, last, err := ac.sendBatch(ctx, ac.after)
		if err != nil {
			return err
		}
		if n < ac.batchSize() {
			// Everything has been read, so start from the oldest row next time to
			// retry what was refused.
			ac.after = 0
			return nil
		}
		ac.after = last
	}
	return nil
}

func (ac *activityCron) batchSize() int {
	if ac.max <= 0 {
		return 100
	}
	return ac.max
}

// sendBatch sends the oldest stored activity with an ID greater than after to
// the Panel, and returns how many rows were read and the ID of the last one.
func (ac *activityCron) sendBatch(ctx context.Context, after int) (int, int, error) {
	var activity []models.Activity
	tx := database.Instance().WithContext(ctx).
		Where("event NOT LIKE ?", "server:sftp.%").
		Where("id > ?", after).
		Order("id ASC").
		Limit(ac.batchSize()).
		Find(&activity)
	if tx.Error != nil {
		return 0, after, errors.WithStack(tx.Error)
	}
	if len(activity) == 0 {
		return 0, after, nil
	}
	last := activity[len(activity)-1].ID

	// ids of activity to delete without sending it.
	var invalid []int
	items := make([]batchItem, 0, len(activity))
	for _, v := range activity {
		// Delete any activity that has an invalid IP address. This is a fix for
		// a bug that truncated the last octet of an IPv6 address in the database.
		if ip := net.ParseIP(v.IP); ip == nil {
			invalid = append(invalid, v.ID)
			continue
		}
		items = append(items, batchItem{activity: v, ids: []int{v.ID}})
	}

	done, err := deliver(ctx, ac.manager.Client(), items)
	if derr := deleteActivity(ctx, append(invalid, done...)); derr != nil {
		return 0, last, derr
	}
	if err != nil {
		return 0, last, errors.WrapIf(err, "cron: failed to send activity events to Panel")
	}
	return len(activity), last, nil
}
