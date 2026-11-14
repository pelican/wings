package external

import (
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
