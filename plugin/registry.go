package plugin

import (
	"context"
	"sync"

	"github.com/apex/log"

	"github.com/pelican/wings/config"
)

// Registry holds the set of registered capability providers. It is safe for
// concurrent use. A process-wide default registry is available via the package
// level helpers (RegisterEventSink, Emit).
type Registry struct {
	mu    sync.RWMutex
	sinks map[string]EventSink // keyed by provider name
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		sinks: make(map[string]EventSink),
	}
}

var defaultRegistry = NewRegistry()

// Default returns the process-wide registry.
func Default() *Registry { return defaultRegistry }

// RegisterEventSink adds an event sink to the default registry.
func RegisterEventSink(s EventSink) { defaultRegistry.RegisterEventSink(s) }

// RegisterEventSink adds an event sink to this registry.
func (r *Registry) RegisterEventSink(s EventSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sinks[s.Name()] = s
}

func (r *Registry) eventSinks() []EventSink {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]EventSink, 0, len(r.sinks))
	for _, s := range r.sinks {
		out = append(out, s)
	}
	return out
}

// Emit delivers e to every enabled event sink whose subscription matches, on the
// default registry.
func Emit(e Event) { defaultRegistry.Emit(e) }

// Emit delivers e to matching, enabled sinks. Delivery is asynchronous and
// best-effort: a sink error is logged and does not affect other sinks or the
// caller. This keeps a slow or broken plugin from stalling the server event
// loop that produced the event.
func (r *Registry) Emit(e Event) {
	for _, s := range r.eventSinks() {
		if !providerEnabled(s.Name()) || !e.matches(s.Events()) {
			continue
		}
		go func(sink EventSink) {
			if err := sink.OnEvent(context.Background(), e); err != nil {
				log.WithField("plugin", sink.Name()).
					WithField("event", e.Type).
					WithError(err).
					Warn("plugin event sink returned an error")
			}
		}(s)
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
