package cron

import (
	"context"

	"github.com/apex/log"
	"github.com/go-co-op/gocron/v2"

	"github.com/pelican/wings/plugin"
	"github.com/pelican/wings/system"
)

// schedulePluginTasks registers every native plugin task on the shared
// scheduler. Each task runs on its own interval; a run is skipped if the
// previous one is still going, a returned error is logged, and a panic is
// recovered so one bad task cannot take down the scheduler.
func schedulePluginTasks(ctx context.Context, s gocron.Scheduler) {
	l := log.WithField("subsystem", "cron")
	for _, t := range plugin.Tasks() {
		if !plugin.Enabled(t.Plugin()) {
			l.WithField("plugin", t.Plugin()).WithField("task", t.Name).
				Info("plugin is disabled; not scheduling its task")
			continue
		}
		if t.Interval <= 0 || t.Run == nil {
			l.WithField("plugin", t.Plugin()).WithField("task", t.Name).
				Warn("skipping plugin task with no interval or handler")
			continue
		}

		task := t
		running := system.NewAtomicBool(false)
		tl := l.WithField("plugin", task.Plugin()).WithField("task", task.Name)

		_, err := s.NewJob(gocron.DurationJob(task.Interval), gocron.NewTask(func() {
			if !running.SwapIf(true) {
				tl.Warn("plugin task still running, skipping this tick")
				return
			}
			defer running.Store(false)
			defer func() {
				if r := recover(); r != nil {
					tl.WithField("panic", r).Error("plugin task panicked")
				}
			}()
			if err := task.Run(ctx); err != nil {
				tl.WithField("error", err).Error("plugin task returned an error")
			}
		}), gocron.WithTags("plugin"))
		if err != nil {
			tl.WithField("error", err).Error("failed to schedule plugin task")
			continue
		}

		tl.WithField("interval", task.Interval).Info("scheduled plugin task")
	}
}
