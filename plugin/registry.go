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
// level helpers (RegisterEventListener, Emit).
type Registry struct {
	mu        sync.RWMutex
	listeners map[string]*listenerWorker // keyed by provider name
}

// listenerQueueSize bounds the events waiting for one listener. A listener that falls this
// far behind loses events rather than holding memory and goroutines without
// limit. A variable so tests can lower it.
var listenerQueueSize = 256

// listenerWorker delivers events to one listener, in order, from its own goroutine.
type listenerWorker struct {
	listener EventListener
	queue    chan Event
	dropped  atomic.Uint64
}

func newListenerWorker(s EventListener) *listenerWorker {
	w := &listenerWorker{listener: s, queue: make(chan Event, listenerQueueSize)}
	go w.run()
	return w
}

func (w *listenerWorker) run() {
	for e := range w.queue {
		w.deliver(e)
	}
}

func (w *listenerWorker) deliver(e Event) {
	l := log.WithField("plugin", w.listener.Name()).WithField("event", e.Type)
	defer func() {
		if p := recover(); p != nil {
			l.WithField("panic", p).Error("plugin event listener panicked")
		}
	}()
	if err := w.listener.OnEvent(context.Background(), e); err != nil {
		l.WithError(err).Warn("plugin event listener returned an error")
	}
}

// offer queues e without blocking and reports whether it was accepted.
func (w *listenerWorker) offer(e Event) bool {
	select {
	case w.queue <- e:
		return true
	default:
		// Log the first drop and then every 1000th, so a stuck plugin on a
		// busy node does not flood the log.
		if n := w.dropped.Add(1); n == 1 || n%1000 == 0 {
			log.WithField("plugin", w.listener.Name()).WithField("dropped", n).
				Warn("plugin event listener is not keeping up; dropping events")
		}
		return false
	}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		listeners: make(map[string]*listenerWorker),
	}
}

var defaultRegistry = NewRegistry()

// Default returns the process-wide registry.
func Default() *Registry { return defaultRegistry }

// RegisterEventListener adds an event listener to the default registry.
func RegisterEventListener(s EventListener) { defaultRegistry.RegisterEventListener(s) }

// RegisterEventListener adds an event listener to this registry. A listener registered
// under an existing name replaces it; the old one finishes what it has queued.
func (r *Registry) RegisterEventListener(s EventListener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.listeners[s.Name()]; ok {
		close(old.queue)
	}
	r.listeners[s.Name()] = newListenerWorker(s)
}

// EventListener returns the listener registered under name, if any.
func (r *Registry) EventListener(name string) (EventListener, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.listeners[name]
	if !ok {
		return nil, false
	}
	return w.listener, true
}

// RemoveEventListener unregisters s if it is still the listener registered
// under its name, and reports whether it was. A listener that has since been
// replaced under the same name stays. s must be of a comparable type. The
// listener gets no new events; what it has queued is still delivered.
func (r *Registry) RemoveEventListener(s EventListener) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.listeners[s.Name()]
	if !ok || w.listener != s {
		return false
	}
	close(w.queue)
	delete(r.listeners, s.Name())
	return true
}

func (r *Registry) eventListeners() []EventListener {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]EventListener, 0, len(r.listeners))
	for _, w := range r.listeners {
		out = append(out, w.listener)
	}
	return out
}

// Emit delivers e to every enabled event listener whose subscription matches, on the
// default registry.
func Emit(e Event) { defaultRegistry.Emit(e) }

// Emit queues e for each matching, enabled listener and returns without waiting.
// Each listener receives events in order on its own goroutine; a listener error or
// panic is logged and affects no other listener. A listener whose queue is full drops
// the event, so a slow or stuck plugin cannot stall the listener that produced
// it or make Wings hold an unbounded backlog.
func (r *Registry) Emit(e Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Check what each listener wants first, and read the policy at most once
	// per event: config.Get copies the whole Wings configuration, and this
	// runs for every server on every stats tick.
	var policy *config.PluginConfiguration
	for _, w := range r.listeners {
		if !wants(e, w.listener.Events()) {
			continue
		}
		if policy == nil {
			p := config.Get().Plugins
			policy = &p
		}
		if ok, _ := decide(*policy, w.listener.Name()); !ok {
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
	return decide(config.Get().Plugins, name)
}

// decide applies the enable policy c to a provider name.
func decide(c config.PluginConfiguration, name string) (bool, string) {
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

// snapshot returns the registered event listeners as a slice for read-only
// inspection.
func (r *Registry) snapshot() []EventListener { return r.eventListeners() }
