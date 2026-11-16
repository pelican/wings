package cron

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

func TestDisabledPluginTasksAreNotScheduled(t *testing.T) {
	c, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = "test-token"
	c.Plugins.Disabled = []string{"cron-off"}
	config.Set(c)

	noop := func(context.Context) error { return nil }
	plugin.RegisterTask("cron-on", plugin.Task{Name: "tick", Interval: time.Hour, Run: noop})
	plugin.RegisterTask("cron-off", plugin.Task{Name: "tick", Interval: time.Hour, Run: noop})

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	schedulePluginTasks(context.Background(), s)

	if n := len(s.Jobs()); n != 1 {
		t.Fatalf("scheduled %d plugin jobs, want 1 (the disabled plugin's task must be skipped)", n)
	}
}
