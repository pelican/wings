package plugin

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/pelican/wings/config"
)

type snapshotListener struct {
	name   string
	events []string
}

func (f snapshotListener) Name() string                         { return f.name }
func (f snapshotListener) Events() []string                     { return f.events }
func (f snapshotListener) OnEvent(context.Context, Event) error { return nil }

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

func TestRegistrySnapshotListsListeners(t *testing.T) {
	setConfig(t)

	r := NewRegistry()
	r.RegisterEventListener(snapshotListener{name: "a", events: []string{"*"}})
	r.RegisterEventListener(snapshotListener{name: "b", events: []string{"server.state"}})
	// A duplicate name overwrites, so a built-in can be shadowed on purpose.
	r.RegisterEventListener(snapshotListener{name: "a", events: []string{"server.install"}})

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

// blockingListener never returns from OnEvent until release is closed.
type blockingListener struct {
	name    string
	release chan struct{}
	got     chan Event
}

func (b blockingListener) Name() string     { return b.name }
func (b blockingListener) Events() []string { return []string{"*"} }
func (b blockingListener) OnEvent(_ context.Context, e Event) error {
	b.got <- e
	<-b.release
	return nil
}

func TestEmitStaysBoundedWhenAListenerIsStuck(t *testing.T) {
	setConfig(t)
	prev := listenerQueueSize
	listenerQueueSize = 8
	t.Cleanup(func() { listenerQueueSize = prev })

	r := NewRegistry()
	stuck := blockingListener{name: "stuck", release: make(chan struct{}), got: make(chan Event, 1)}
	defer close(stuck.release)
	r.RegisterEventListener(stuck)

	before := runtime.NumGoroutine()
	start := time.Now()
	for i := 0; i < 10000; i++ {
		r.Emit(Event{Type: "server.stats", Server: "abc"})
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Emit blocked on a stuck listener for %v", d)
	}
	if grew := runtime.NumGoroutine() - before; grew > 5 {
		t.Fatalf("emitting to a stuck listener started %d goroutines", grew)
	}
}

type orderListener struct{ got chan string }

func (o orderListener) Name() string     { return "order" }
func (o orderListener) Events() []string { return []string{"*"} }
func (o orderListener) OnEvent(_ context.Context, e Event) error {
	if e.Type == "panic" {
		panic("boom")
	}
	o.got <- e.Type
	return nil
}

func TestListenerGetsEventsInOrderAndSurvivesAPanic(t *testing.T) {
	setConfig(t)
	r := NewRegistry()
	s := orderListener{got: make(chan string, 4)}
	r.RegisterEventListener(s)

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
			t.Fatalf("event %q never arrived; a panicking event should not stop the listener", want)
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

// parkedListener wants every event type in events and blocks in its first
// OnEvent until release closes, so its worker allocates nothing meanwhile.
type parkedListener struct {
	name    string
	events  []string
	release chan struct{}
}

func (p parkedListener) Name() string     { return p.name }
func (p parkedListener) Events() []string { return p.events }
func (p parkedListener) OnEvent(context.Context, Event) error {
	<-p.release
	return nil
}

// TestEmitReadsThePolicyOncePerEvent covers the stats tick: Emit used to copy
// the whole Wings configuration once per registered listener, including
// listeners that don't want the event. It must cost the same with 40
// listeners as with one.
func TestEmitReadsThePolicyOncePerEvent(t *testing.T) {
	setConfig(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r := NewRegistry()
	for i := 0; i < 20; i++ {
		r.RegisterEventListener(parkedListener{name: fmt.Sprintf("wants-%d", i), events: []string{"server.stats"}, release: release})
		r.RegisterEventListener(parkedListener{name: fmt.Sprintf("other-%d", i), events: []string{"server.state"}, release: release})
	}
	e := Event{Type: "server.stats", Server: "x"}
	r.Emit(e) // each wanting worker takes this one and parks

	if allocs := testing.AllocsPerRun(50, func() { r.Emit(e) }); allocs > 2 {
		t.Fatalf("Emit with 40 listeners made %.0f allocations, want at most 2 (one policy read)", allocs)
	}
}

// countingListener records events; a pointer, so it is comparable.
type countingListener struct {
	name string
	got  chan Event
}

func (c *countingListener) Name() string     { return c.name }
func (c *countingListener) Events() []string { return []string{"*"} }
func (c *countingListener) OnEvent(_ context.Context, e Event) error {
	c.got <- e
	return nil
}

// TestRemoveEventListenerOnlyRemovesThatListener covers how the external
// loader stops a plugin: it removes the listener it registered, and must not
// remove one registered under the same name since.
func TestRemoveEventListenerOnlyRemovesThatListener(t *testing.T) {
	setConfig(t)
	r := NewRegistry()
	first := &countingListener{name: "p", got: make(chan Event, 4)}
	second := &countingListener{name: "p", got: make(chan Event, 4)}

	r.RegisterEventListener(first)
	if l, ok := r.EventListener("p"); !ok || l != first {
		t.Fatalf("EventListener(p) = %v, %v; want the registered listener", l, ok)
	}
	r.RegisterEventListener(second)
	if r.RemoveEventListener(first) {
		t.Fatal("removed p although the registered listener is a newer one")
	}
	if l, _ := r.EventListener("p"); l != second {
		t.Fatal("the newer listener was removed")
	}
	if !r.RemoveEventListener(second) {
		t.Fatal("did not remove the registered listener")
	}
	if _, ok := r.EventListener("p"); ok {
		t.Fatal("p is still registered")
	}
	r.Emit(Event{Type: "server.state"})
	select {
	case e := <-second.got:
		t.Fatalf("a removed listener got %v", e)
	case <-time.After(100 * time.Millisecond):
	}
	if r.RemoveEventListener(second) {
		t.Fatal("removing twice reported success")
	}
}
