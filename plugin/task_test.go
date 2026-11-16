package plugin

import (
	"context"
	"testing"
	"time"
)

func TestRegisterTaskSetsOwnerAndCollects(t *testing.T) {
	before := len(Tasks())

	noop := func(context.Context) error { return nil }
	RegisterTask("reaper",
		Task{Name: "reap", Interval: time.Minute, Run: noop},
		Task{Name: "sweep", Interval: 30 * time.Second, Run: noop},
	)

	got := Tasks()
	if len(got) != before+2 {
		t.Fatalf("expected %d tasks, got %d", before+2, len(got))
	}
	for _, task := range got[before:] {
		if task.Plugin() != "reaper" {
			t.Fatalf("expected task owner %q, got %q", "reaper", task.Plugin())
		}
	}
	if got[before].Name != "reap" || got[before+1].Interval != 30*time.Second {
		t.Fatalf("task fields not preserved: %+v %+v", got[before], got[before+1])
	}
}
