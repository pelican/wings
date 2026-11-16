package plugin

import (
	"context"
	"time"
)

// Task is a periodic job a native plugin registers. Wings runs it on the shared
// cron scheduler at the given Interval; a task never overlaps with itself, and a
// returned error is logged without stopping the schedule.
type Task struct {
	// Name identifies the task in logs.
	Name string
	// Interval is how often Run is invoked. Must be greater than zero.
	Interval time.Duration
	// Run performs the work for one tick.
	Run func(ctx context.Context) error

	// plugin is the owning plugin name; set by RegisterTask.
	plugin string
}

// Plugin returns the name of the plugin that owns this task.
func (t Task) Plugin() string { return t.plugin }

// tasks holds every native plugin task, populated from plugin init() functions
// via RegisterTask. It is read once at startup when the scheduler is built, so
// no locking is required.
var tasks []Task

// RegisterTask adds periodic tasks for a plugin. Call it from a provider's
// init(), the same way native providers and routes register themselves:
//
//	func init() {
//	    plugin.RegisterTask("reaper", plugin.Task{
//	        Name:     "reap",
//	        Interval: time.Minute,
//	        Run:      func(ctx context.Context) error { /* ... */ return nil },
//	    })
//	}
func RegisterTask(pluginName string, ts ...Task) {
	for _, t := range ts {
		t.plugin = pluginName
		tasks = append(tasks, t)
	}
}

// Tasks returns the registered native plugin tasks for the scheduler to mount.
func Tasks() []Task { return tasks }
