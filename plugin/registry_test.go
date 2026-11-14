package plugin

import (
	"context"
	"testing"

	"github.com/pelican/wings/config"
)

type snapshotSink struct {
	name   string
	events []string
}

func (f snapshotSink) Name() string                         { return f.name }
func (f snapshotSink) Events() []string                     { return f.events }
func (f snapshotSink) OnEvent(context.Context, Event) error { return nil }

// setConfig installs a fresh global configuration for a test and returns it for
// further mutation via config.Set.
func setConfig(t *testing.T) *config.Configuration {
	t.Helper()
	c, err := config.NewAtPath("test.yml")
	if err != nil {
		t.Fatalf("config.NewAtPath: %v", err)
	}
	// config.Set derives a JWT signing key from the token and panics on an empty
	// one, so give the test configuration a non-empty token.
	c.AuthenticationToken = "test-token"
	config.Set(c)
	return c
}

func TestRegistrySnapshotListsSinks(t *testing.T) {
	setConfig(t)

	r := NewRegistry()
	r.RegisterEventSink(snapshotSink{name: "a", events: []string{"*"}})
	r.RegisterEventSink(snapshotSink{name: "b", events: []string{"server.state"}})
	// A duplicate name overwrites, so a built-in can be shadowed on purpose.
	r.RegisterEventSink(snapshotSink{name: "a", events: []string{"server.install"}})

	got := map[string][]string{}
	for _, s := range r.snapshot() {
		got[s.Name()] = s.Events()
	}
	if len(got) != 2 || got["a"][0] != "server.install" || got["b"][0] != "server.state" {
		t.Fatalf("snapshot = %v", got)
	}
}

func TestProviderEnabledAllowlistLocksDown(t *testing.T) {
	c := setConfig(t)
	c.Plugins.Autoload = true
	c.Plugins.Enabled = []string{"only-this"}
	config.Set(c)

	if providerEnabled("something-else") {
		t.Fatal("provider outside allowlist should be disabled")
	}
	if !providerEnabled("only-this") {
		t.Fatal("allowlisted provider should be enabled")
	}
}

func TestProviderEnabledAutoloadOff(t *testing.T) {
	c := setConfig(t)
	c.Plugins.Autoload = false
	c.Plugins.Enabled = nil
	config.Set(c)

	if providerEnabled("anything") {
		t.Fatal("with autoload off and no allowlist, nothing should be enabled")
	}
}
