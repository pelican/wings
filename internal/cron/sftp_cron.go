package cron

import (
	"context"
	"encoding/json"
	"net"
	"reflect"

	"emperror.dev/errors"

	"github.com/pelican/wings/internal/database"
	"github.com/pelican/wings/internal/models"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/system"
)

const (
	// maxFilesPerEvent is the most files listed in one merged SFTP event. Further
	// files for the same group start a new event.
	maxFilesPerEvent = 500
	// maxRequestBytes is roughly the most activity data sent in one request.
	maxRequestBytes = 4 << 20
)

type sftpCron struct {
	mu      *system.AtomicBool
	manager *server.Manager
	max     int
	// after is the ID before the next row to read. Rows the Panel refuses stay
	// stored, so reading carries on from here, across runs, rather than from the
	// first row. It goes back to zero once the end is reached, so refused rows
	// are still retried.
	after int
}

type mapKey struct {
	User      string
	Server    string
	IP        string
	Event     models.Event
	Timestamp string
	// Part separates the events for a group that has more files than fit in one.
	Part int
}

type eventGroup struct {
	activity *models.Activity
	ids      []int
	files    int
}

type eventMap struct {
	max   int
	bytes int
	m     map[mapKey]*eventGroup
}

// Run executes the SFTP reconciliation cron. This job will pull all of the SFTP specific events
// and merge them together across user, server, ip, and event. This allows a SFTP event that deletes
// tens or hundreds of files to be tracked as a single "deletion" event so long as they all occur
// within the same one minute period of time (starting at the first timestamp for the group). Without
// this we'd end up flooding the Panel event log with excessive data that is of no use to end users.
func (sc *sftpCron) Run(ctx context.Context) error {
	if !sc.mu.SwapIf(true) {
		return errors.WithStack(ErrCronRunning)
	}
	defer sc.mu.Store(false)

	for i := 0; i < maxBatchesPerRun; i++ {
		more, err := sc.sendBatch(ctx)
		if err != nil {
			return err
		}
		if !more {
			// Everything has been read, so start from the first row next time to
			// retry what was refused.
			sc.after = 0
			return nil
		}
	}
	return nil
}

// sendBatch merges as many stored SFTP events as fit in one request, starting
// after sc.after, and sends them to the Panel. It reports whether there were
// more events than fit.
func (sc *sftpCron) sendBatch(ctx context.Context) (bool, error) {
	events := &eventMap{m: map[mapKey]*eventGroup{}, max: sc.max}
	if events.max <= 0 {
		events.max = 100
	}

	// ids of activity to delete without sending it.
	var invalid []int
	// The ID of the first row that did not fit, which the next batch starts at.
	firstLeft := 0
	last := sc.after
	for {
		activity, err := sc.fetchRecords(ctx, last, events.max)
		if err != nil {
			return false, err
		}
		if len(activity) == 0 {
			break
		}
		last = activity[len(activity)-1].ID

		added := false
		for _, a := range activity {
			// The Panel refuses activity with an IP address it cannot parse.
			if net.ParseIP(a.IP) == nil {
				invalid = append(invalid, a.ID)
				continue
			}
			if events.Push(a) {
				added = true
			} else if firstLeft == 0 {
				firstLeft = a.ID
			}
		}
		// Stop once a page adds nothing, since every later event belongs in
		// another request.
		if !added {
			break
		}
	}

	done, err := deliver(ctx, sc.manager.Client(), events.Items())
	if derr := deleteActivity(ctx, append(invalid, done...)); derr != nil {
		return false, derr
	}
	if err != nil {
		return false, errors.Wrap(err, "failed to send sftp activity logs to Panel")
	}
	// Carry on after everything that was sent, or refused and kept, so that the
	// next batch reaches later activity. The first row always fits, so this
	// always moves forward.
	if firstLeft == 0 {
		sc.after = last
		return false, nil
	}
	sc.after = firstLeft - 1
	return true, nil
}

// fetchRecords returns up to limit SFTP activity events with an ID greater than
// after, oldest first. Several queries may be needed to fill a request, since
// this cron merges any activity that lines up across user, server, ip, and event
// into a single activity record when sending the data to the Panel.
func (sc *sftpCron) fetchRecords(ctx context.Context, after int, limit int) (activity []models.Activity, err error) {
	tx := database.Instance().WithContext(ctx).
		Where("event LIKE ?", "server:sftp.%").
		Where("id > ?", after).
		Order("id ASC").
		Limit(limit).
		Find(&activity)
	if tx.Error != nil {
		err = errors.WithStack(tx.Error)
	}
	return
}

// Push adds an activity to the event mapping, or de-duplicates it and merges the files metadata
// into the existing entity that exists. It returns false if the activity does not fit in this
// request.
func (em *eventMap) Push(a models.Activity) bool {
	size := activitySize(a)
	if em.bytes > 0 && em.bytes+size > maxRequestBytes {
		return false
	}
	g := em.forActivity(a)
	if g == nil {
		return false
	}
	em.bytes += size
	g.ids = append(g.ids, a.ID)
	m := g.activity
	// Always reduce this to the first timestamp that was recorded for the set
	// of events, and not
	if a.Timestamp.Before(m.Timestamp) {
		m.Timestamp = a.Timestamp
	}
	list := m.Metadata["files"].([]interface{})
	if s, ok := a.Metadata["files"]; ok {
		v := reflect.ValueOf(s)
		if v.Kind() != reflect.Slice || v.IsNil() {
			return true
		}
		for i := 0; i < v.Len(); i++ {
			list = append(list, v.Index(i).Interface())
		}
		g.files += v.Len()
		// You must set it again at the end of the process, otherwise you've only updated the file
		// slice in this one loop since it isn't passed by reference. This is just shorter than having
		// to explicitly keep casting it to the slice.
		m.Metadata["files"] = list
	}
	return true
}

// Items returns the merged events with the IDs of the activity they were made
// from.
func (em *eventMap) Items() []batchItem {
	out := make([]batchItem, 0, len(em.m))
	for _, g := range em.m {
		out = append(out, batchItem{activity: *g.activity, ids: g.ids})
	}
	return out
}

// forActivity returns an event entity from our map which allows existing matches to be
// updated with additional files.
func (em *eventMap) forActivity(a models.Activity) *eventGroup {
	key := mapKey{
		User:   a.User.String,
		Server: a.Server,
		IP:     a.IP,
		Event:  a.Event,
		// We group by the minute, don't care about the seconds for this logic.
		Timestamp: a.Timestamp.Format("2006-01-02_15:04"),
	}
	for {
		v, ok := em.m[key]
		if !ok {
			break
		}
		if v.files < maxFilesPerEvent {
			return v
		}
		key.Part++
	}
	// Cap the size of the events map at the defined maximum events to send to the Panel. Just
	// return nil and let the caller handle it.
	if len(em.m) >= em.max {
		return nil
	}
	// Doesn't exist in our map yet, create a copy of the activity passed into this
	// function and then assign it into the map with an empty metadata value.
	v := a
	v.Metadata = models.ActivityMeta{
		"files": make([]interface{}, 0),
	}
	g := &eventGroup{activity: &v}
	em.m[key] = g
	return g
}

// activitySize estimates how much an activity adds to a request.
func activitySize(a models.Activity) int {
	b, _ := json.Marshal(a.Metadata)
	return len(b) + 256
}
