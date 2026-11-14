package plugin

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/apex/log"

	"github.com/pelican/wings/config"
)

// Registry holds the set of registered capability providers. It is safe for
// concurrent use. A process-wide default registry is available via the package
// level helpers (RegisterEventSink, Emit).
type Registry struct {
	mu    sync.RWMutex
	sinks map[string]*sinkWorker // keyed by provider name
}

// sinkQueueSize bounds the events waiting for one sink. A sink that falls this
// far behind loses events rather than holding memory and goroutines without
// limit. A variable so tests can lower it.
var sinkQueueSize = 256

// sinkWorker delivers events to one sink, in order, from its own goroutine.
type sinkWorker struct {
	sink    EventSink
	queue   chan Event
	dropped atomic.Uint64
}

func newSinkWorker(s EventSink) *sinkWorker {
	w := &sinkWorker{sink: s, queue: make(chan Event, sinkQueueSize)}
	go w.run()
	return w
}

func (w *sinkWorker) run() {
	for e := range w.queue {
		w.deliver(e)
	}
}

func (w *sinkWorker) deliver(e Event) {
	l := log.WithField("plugin", w.sink.Name()).WithField("event", e.Type)
	defer func() {
		if p := recover(); p != nil {
			l.WithField("panic", p).Error("plugin event sink panicked")
		}
	}()
	if err := w.sink.OnEvent(context.Background(), e); err != nil {
		l.WithError(err).Warn("plugin event sink returned an error")
	}
}

// offer queues e without blocking and reports whether it was accepted.
func (w *sinkWorker) offer(e Event) bool {
	select {
	case w.queue <- e:
		return true
	default:
		// Log the first drop and then every 1000th, so a stuck plugin on a
		// busy node does not flood the log.
		if n := w.dropped.Add(1); n == 1 || n%1000 == 0 {
			log.WithField("plugin", w.sink.Name()).WithField("dropped", n).
				Warn("plugin event sink is not keeping up; dropping events")
		}
		return false
	}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		sinks: make(map[string]*sinkWorker),
	}
}

var defaultRegistry = NewRegistry()

// Default returns the process-wide registry.
func Default() *Registry { return defaultRegistry }

// RegisterEventSink adds an event sink to the default registry.
func RegisterEventSink(s EventSink) { defaultRegistry.RegisterEventSink(s) }

// RegisterEventSink adds an event sink to this registry. A sink registered
// under an existing name replaces it; the old one finishes what it has queued.
func (r *Registry) RegisterEventSink(s EventSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.sinks[s.Name()]; ok {
		close(old.queue)
	}
	r.sinks[s.Name()] = newSinkWorker(s)
}

func (r *Registry) eventSinks() []EventSink {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]EventSink, 0, len(r.sinks))
	for _, w := range r.sinks {
		out = append(out, w.sink)
	}
	return out
}

// Emit delivers e to every enabled event sink whose subscription matches, on the
// default registry.
func Emit(e Event) { defaultRegistry.Emit(e) }

// Emit queues e for each matching, enabled sink and returns without waiting.
// Each sink receives events in order on its own goroutine; a sink error or
// panic is logged and affects no other sink. A sink whose queue is full drops
// the event, so a slow or stuck plugin cannot stall the listener that produced
// it or make Wings hold an unbounded backlog.
func (r *Registry) Emit(e Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, w := range r.sinks {
		if !providerEnabled(w.sink.Name()) || !e.matches(w.sink.Events()) {
			continue
		}
		w.offer(e)
	}
}

// providerEnabled reports whether a provider name is active under the current
// configuration policy.
func providerEnabled(name string) bool {
	enabled, _ := enableState(name)
	return enabled
}

// Enabled reports whether the named provider is active under current policy. It
// is the exported form of providerEnabled for use by transport packages.
func Enabled(name string) bool { return providerEnabled(name) }

// enableState resolves the policy decision for a provider name and a
// human-readable reason. Precedence: an explicit Disabled blocklist wins; then a
// non-empty Enabled allowlist; otherwise the Autoload default.
func enableState(name string) (bool, string) {
	c := config.Get().Plugins
	for _, n := range c.Disabled {
		if n == name {
			return false, "blocklist"
		}
	}
	if len(c.Enabled) > 0 {
		for _, n := range c.Enabled {
			if n == name {
				return true, "allowlist"
			}
		}
		return false, "not in allowlist"
	}
	if c.Autoload {
		return true, "autoload"
	}
	return false, "autoload off"
}

// snapshot returns the registered event sinks as a slice for read-only
// inspection.
func (r *Registry) snapshot() []EventSink { return r.eventSinks() }
