package plugin

import (
	"context"
	"runtime"
	"testing"
	"time"

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

// blockingSink never returns from OnEvent until release is closed.
type blockingSink struct {
	name    string
	release chan struct{}
	got     chan Event
}

func (b blockingSink) Name() string     { return b.name }
func (b blockingSink) Events() []string { return []string{"*"} }
func (b blockingSink) OnEvent(_ context.Context, e Event) error {
	b.got <- e
	<-b.release
	return nil
}

func TestEmitStaysBoundedWhenASinkIsStuck(t *testing.T) {
	setConfig(t)
	prev := sinkQueueSize
	sinkQueueSize = 8
	t.Cleanup(func() { sinkQueueSize = prev })

	r := NewRegistry()
	stuck := blockingSink{name: "stuck", release: make(chan struct{}), got: make(chan Event, 1)}
	defer close(stuck.release)
	r.RegisterEventSink(stuck)

	before := runtime.NumGoroutine()
	start := time.Now()
	for i := 0; i < 10000; i++ {
		r.Emit(Event{Type: "server.stats", Server: "abc"})
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Emit blocked on a stuck sink for %v", d)
	}
	if grew := runtime.NumGoroutine() - before; grew > 5 {
		t.Fatalf("emitting to a stuck sink started %d goroutines", grew)
	}
}

type orderSink struct{ got chan string }

func (o orderSink) Name() string     { return "order" }
func (o orderSink) Events() []string { return []string{"*"} }
func (o orderSink) OnEvent(_ context.Context, e Event) error {
	if e.Type == "panic" {
		panic("boom")
	}
	o.got <- e.Type
	return nil
}

func TestSinkGetsEventsInOrderAndSurvivesAPanic(t *testing.T) {
	setConfig(t)
	r := NewRegistry()
	s := orderSink{got: make(chan string, 4)}
	r.RegisterEventSink(s)

	for _, typ := range []string{"a", "panic", "b", "c"} {
		r.Emit(Event{Type: typ})
	}
	for _, want := range []string{"a", "b", "c"} {
		select {
		case got := <-s.got:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("event %q never arrived; a panicking event should not stop the sink", want)
		}
	}
}

func TestInventoryListsRouteAndTaskOnlyPlugins(t *testing.T) {
	c := setConfig(t)
	c.Plugins.Directory = t.TempDir()
	config.Set(c)
	RegisterRoutes("inv-routes", Route{Scope: ScopeNode, Method: "GET", Path: "/x"})
	RegisterTask("inv-tasks", Task{Name: "t", Interval: time.Minute})

	inv, err := Inventory()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, p := range inv {
		seen[p.Name]++
	}
	if seen["inv-routes"] != 1 || seen["inv-tasks"] != 1 {
		t.Fatalf("route-only and task-only plugins should each be listed once, got %v", seen)
	}
}
