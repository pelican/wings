package plugin

import "context"

// CapabilityEvents covers plugins that subscribe to server lifecycle events.
const CapabilityEvents Capability = "events"

// Event is a server lifecycle notification delivered to event listeners.
type Event struct {
	Type   string         `json:"type"`   // e.g. "server.state", "server.install"
	Server string         `json:"server"` // server UUID
	Data   map[string]any `json:"data,omitempty"`
}

// EventListener receives lifecycle events. Implementations may be native (in-process)
// or external (a subprocess that receives an "event" notification).
type EventListener interface {
	Name() string
	// Events returns the event types this listener wants, or []string{"*"} for all.
	Events() []string
	OnEvent(ctx context.Context, e Event) error
}

// matches reports whether the event should be delivered given a listener's wanted
// event types.
func (e Event) matches(want []string) bool {
	for _, w := range want {
		if w == "*" || w == e.Type {
			return true
		}
	}
	return false
}
