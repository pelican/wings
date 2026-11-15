// Package contract is what a Wings plugin and Wings agree on: the event a
// plugin receives and the interface it implements. It imports only the
// standard library, so a plugin built against it (directly or through
// plugin/sdk) never pulls in Wings itself.
//
// Native plugins use the same types through package plugin, which re-exports
// them; the two are interchangeable.
package contract

import "context"

// Capability identifies a category of plugin functionality. New capabilities
// are added as a typed Go interface plus a constant here.
type Capability string

// CapabilityEvents covers plugins that subscribe to server lifecycle events.
const CapabilityEvents Capability = "events"

// Event is a server lifecycle notification delivered to event listeners.
type Event struct {
	Type   string         `json:"type"`   // e.g. "server.state", "server.install"
	Server string         `json:"server"` // server UUID
	Data   map[string]any `json:"data,omitempty"`
}

// EventListener receives lifecycle events. Implementations may be native
// (in-process) or external (a subprocess that receives an "event"
// notification).
type EventListener interface {
	Name() string
	// Events returns the event types this listener wants, or []string{"*"} for all.
	Events() []string
	OnEvent(ctx context.Context, e Event) error
}
