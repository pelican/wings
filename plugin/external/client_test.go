package external

import (
	"strings"
	"testing"

	"github.com/pelican/wings/plugin"
)

func TestEventSinkScopeComesFromManifest(t *testing.T) {
	c := NewClient(plugin.Manifest{
		Name:       "audit",
		Capability: plugin.CapabilityEvents,
		Provides: plugin.Provides{
			Events: []string{"server.state"},
		},
	}, nil)

	s := c.EventSink()
	if s.Name() != "audit" {
		t.Fatalf("name = %q, want manifest name", s.Name())
	}
	got := s.Events()
	if len(got) != 1 || got[0] != "server.state" {
		t.Fatalf("events = %v, want manifest scope only", got)
	}
}

func TestEventSinkDefaultsToAllEvents(t *testing.T) {
	c := NewClient(plugin.Manifest{Name: "audit", Capability: plugin.CapabilityEvents}, nil)
	got := c.EventSink().Events()
	if len(got) != 1 || got[0] != "*" {
		t.Fatalf("events = %v, want [*] when the manifest names none", got)
	}
}

func TestPipeLinesKeepsReadingAfterAnOversizedLine(t *testing.T) {
	long := strings.Repeat("x", 100<<10)
	input := "first\n" + long + "\nafter\n"
	var got []string
	pipeLines(strings.NewReader(input), 1024, func(l string) { got = append(got, l) })

	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(got), got)
	}
	if got[0] != "first" || got[2] != "after" {
		t.Fatalf("lines around the long one were lost: %q / %q", got[0], got[2])
	}
	if !strings.HasSuffix(got[1], "(line truncated)") || len(got[1]) > 1024+64 {
		t.Fatalf("the long line should be cut and marked, got %d bytes", len(got[1]))
	}
}
