package main

import (
	"context"
	"testing"

	"github.com/pelican/wings/plugin/sdk"
)

func TestCountsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	c, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"running", "offline", "running"} {
		if err := c.OnEvent(context.Background(), sdk.Event{Type: "server.state", Server: "abc", Data: map[string]any{"state": s}}); err != nil {
			t.Fatal(err)
		}
	}
	again, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.counts["abc"]["running"] != 2 || again.counts["abc"]["offline"] != 1 {
		t.Fatalf("counts after reload = %v", again.counts)
	}
}
