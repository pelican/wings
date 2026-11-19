package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/pelican/wings/config"
)

type fakeListener struct {
	name string
	want []string
	got  chan Event
}

func (f *fakeListener) Name() string     { return f.name }
func (f *fakeListener) Events() []string { return f.want }
func (f *fakeListener) OnEvent(_ context.Context, e Event) error {
	f.got <- e
	return nil
}

func TestEmitDeliversToMatchingEnabledListener(t *testing.T) {
	setConfig(t) // Autoload true -> listener enabled

	r := NewRegistry()
	listener := &fakeListener{name: "s", want: []string{"server.state"}, got: make(chan Event, 1)}
	r.RegisterEventListener(listener)

	r.Emit(Event{Type: "server.state", Server: "x"})

	select {
	case e := <-listener.got:
		if e.Server != "x" {
			t.Fatalf("unexpected event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("event was not delivered")
	}
}

func TestEmitSkipsNonMatchingType(t *testing.T) {
	setConfig(t)

	r := NewRegistry()
	listener := &fakeListener{name: "s", want: []string{"other"}, got: make(chan Event, 1)}
	r.RegisterEventListener(listener)

	r.Emit(Event{Type: "server.state"})

	select {
	case <-listener.got:
		t.Fatal("event should not have been delivered")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestEmitSkipsDisabledListener(t *testing.T) {
	c := setConfig(t)
	c.Plugins.Disabled = []string{"s"}
	config.Set(c)

	r := NewRegistry()
	listener := &fakeListener{name: "s", want: []string{"*"}, got: make(chan Event, 1)}
	r.RegisterEventListener(listener)

	r.Emit(Event{Type: "server.state"})

	select {
	case <-listener.got:
		t.Fatal("disabled listener should not receive events")
	case <-time.After(200 * time.Millisecond):
	}
}
