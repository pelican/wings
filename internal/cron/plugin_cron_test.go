package cron

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// uniqueName returns a plugin name no other test run has used: tasks are
// registered process-wide, and -count reruns a test in the same process.
var runs atomic.Int32

func uniqueName(base string) string { return fmt.Sprintf("%s-%d", base, runs.Add(1)) }

func setPluginPolicy(t *testing.T, disabled ...string) {
	t.Helper()
	c, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = "test-token"
	c.Plugins.Disabled = disabled
	config.Set(c)
}

// runJob runs the scheduled job with the given name once and reports whether
// the task body ran within wait.
func runJob(t *testing.T, s gocron.Scheduler, name string, ran <-chan struct{}, wait time.Duration) bool {
	t.Helper()
	for _, j := range s.Jobs() {
		if j.Name() != name {
			continue
		}
		if err := j.RunNow(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ran:
			return true
		case <-time.After(wait):
			return false
		}
	}
	t.Fatalf("no job named %q was scheduled", name)
	return false
}

// TestPluginTasksFollowTheEnablePolicyLive covers enabling and disabling a
// plugin while Wings runs: its task must start and stop running at the next
// tick, without restarting Wings to rebuild the scheduler.
func TestPluginTasksFollowTheEnablePolicyLive(t *testing.T) {
	name := uniqueName("cron-live")
	setPluginPolicy(t, name)

	ran := make(chan struct{}, 1)
	plugin.RegisterTask(name, plugin.Task{Name: "tick", Interval: time.Hour, Run: func(context.Context) error {
		ran <- struct{}{}
		return nil
	}})

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	s.Start()
	schedulePluginTasks(context.Background(), s)

	if runJob(t, s, name+"/tick", ran, 300*time.Millisecond) {
		t.Fatal("the task of a disabled plugin ran")
	}

	config.Update(func(c *config.Configuration) { c.Plugins.Disabled = nil })
	if !runJob(t, s, name+"/tick", ran, 5*time.Second) {
		t.Fatal("the task did not run after its plugin was enabled")
	}

	config.Update(func(c *config.Configuration) { c.Plugins.Disabled = []string{name} })
	if runJob(t, s, name+"/tick", ran, 300*time.Millisecond) {
		t.Fatal("the task still ran after its plugin was disabled")
	}
}

func TestPluginTasksWithoutAnIntervalAreNotScheduled(t *testing.T) {
	setPluginPolicy(t)
	plugin.RegisterTask("cron-bad", plugin.Task{Name: "tick", Run: func(context.Context) error { return nil }})

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	schedulePluginTasks(context.Background(), s)
	for _, j := range s.Jobs() {
		if j.Name() == "cron-bad/tick" {
			t.Fatal("a task with no interval was scheduled")
		}
	}
}

// TestDisablingAPluginStopsItsTaskLive starts with the plugin enabled, as on
// most nodes, and disables it while Wings runs.
func TestDisablingAPluginStopsItsTaskLive(t *testing.T) {
	name := uniqueName("cron-stop")
	setPluginPolicy(t)

	ran := make(chan struct{}, 1)
	plugin.RegisterTask(name, plugin.Task{Name: "tick", Interval: time.Hour, Run: func(context.Context) error {
		ran <- struct{}{}
		return nil
	}})

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	s.Start()
	schedulePluginTasks(context.Background(), s)

	if !runJob(t, s, name+"/tick", ran, 5*time.Second) {
		t.Fatal("the task of an enabled plugin did not run")
	}
	config.Update(func(c *config.Configuration) { c.Plugins.Disabled = []string{name} })
	if runJob(t, s, name+"/tick", ran, 300*time.Millisecond) {
		t.Fatal("the task still ran after its plugin was disabled")
	}
}
