package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/pelican/wings/config"
)

type fakeSink struct {
	name string
	want []string
	got  chan Event
}

func (f *fakeSink) Name() string     { return f.name }
func (f *fakeSink) Events() []string { return f.want }
func (f *fakeSink) OnEvent(_ context.Context, e Event) error {
	f.got <- e
	return nil
}

func TestEmitDeliversToMatchingEnabledSink(t *testing.T) {
	setConfig(t) // Autoload true -> sink enabled

	r := NewRegistry()
	sink := &fakeSink{name: "s", want: []string{"server.state"}, got: make(chan Event, 1)}
	r.RegisterEventSink(sink)

	r.Emit(Event{Type: "server.state", Server: "x"})

	select {
	case e := <-sink.got:
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
	sink := &fakeSink{name: "s", want: []string{"other"}, got: make(chan Event, 1)}
	r.RegisterEventSink(sink)

	r.Emit(Event{Type: "server.state"})

	select {
	case <-sink.got:
		t.Fatal("event should not have been delivered")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestEmitSkipsDisabledSink(t *testing.T) {
	c := setConfig(t)
	c.Plugins.Disabled = []string{"s"}
	config.Set(c)

	r := NewRegistry()
	sink := &fakeSink{name: "s", want: []string{"*"}, got: make(chan Event, 1)}
	r.RegisterEventSink(sink)

	r.Emit(Event{Type: "server.state"})

	select {
	case <-sink.got:
		t.Fatal("disabled sink should not receive events")
	case <-time.After(200 * time.Millisecond):
	}
}
