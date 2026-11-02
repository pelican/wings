package cron

import (
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"

	"github.com/pelican/wings/plugins"
)

// pluginScheduler adapts the node's gocron scheduler to what the plugin
// subsystem needs.
//
// Plugin jobs cannot be registered up front with the rest of the crons: a
// plugin can be enabled and disabled while Wings runs, and its jobs have to
// come and go with it. This wrapper is handed to the plugin manager so it can
// add and remove jobs on the already-running scheduler.
type pluginScheduler struct {
	scheduler gocron.Scheduler
}

var _ plugins.JobScheduler = (*pluginScheduler)(nil)

// AddPluginJob schedules a plugin's recurring work.
func (p *pluginScheduler) AddPluginJob(pluginID, name string, interval time.Duration, run func()) (string, error) {
	l := log.WithField("subsystem", "cron").WithFields(log.Fields{
		"plugin": pluginID,
		"job":    name,
	})

	job, err := p.scheduler.NewJob(
		gocron.DurationJob(interval),
		gocron.NewTask(run),
		// Runs never overlap. A plugin job that takes longer than its interval
		// would otherwise pile up, and the plugin's own hook lock would then
		// serialise the backlog while blocking everything else that plugin
		// does.
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
		gocron.WithName(pluginID+":"+name),
	)
	if err != nil {
		return "", errors.Wrapf(err, "cron: could not schedule job %s for plugin %s", name, pluginID)
	}

	l.WithField("interval", interval.String()).Debug("scheduled plugin job")

	return job.ID().String(), nil
}

// RemovePluginJob cancels a plugin job by the handle AddPluginJob returned.
func (p *pluginScheduler) RemovePluginJob(handle string) error {
	id, err := uuid.Parse(handle)
	if err != nil {
		return errors.Wrapf(err, "cron: %q is not a plugin job handle", handle)
	}

	if err := p.scheduler.RemoveJob(id); err != nil {
		// A job that is already gone is the state we wanted, which happens
		// when the scheduler is shutting down at the same time as a plugin is
		// being disabled.
		if errors.Is(err, gocron.ErrJobNotFound) {
			return nil
		}
		return errors.Wrap(err, "cron: could not remove plugin job")
	}
	return nil
}

// PluginScheduler wraps a scheduler for the plugin manager to use.
func PluginScheduler(s gocron.Scheduler) plugins.JobScheduler {
	return &pluginScheduler{scheduler: s}
}
