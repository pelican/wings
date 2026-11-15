// Package plugin is Wings' plugin runtime: the registry that delivers events
// to listeners, the hooks native plugins register from init (event
// listeners, HTTP routes, scheduled tasks), the enable policy, the grant check
// on host callbacks, and the manifests and trust store that decide which
// external plugins may run.
//
// The types a plugin implements live in plugin/contract and are re-exported
// here, so a native plugin only needs this package. Installing and updating
// plugins is plugin/install; running external ones is plugin/external.
package plugin

import "github.com/pelican/wings/plugin/contract"

// Capability identifies a category of plugin functionality.
type Capability = contract.Capability

// CapabilityEvents covers plugins that subscribe to server lifecycle events.
const CapabilityEvents = contract.CapabilityEvents

// Event is a server lifecycle notification delivered to event listeners.
type Event = contract.Event

// EventListener receives lifecycle events; see contract.EventListener.
type EventListener = contract.EventListener

// wants reports whether the event should be delivered given a listener's
// wanted event types.
func wants(e Event, want []string) bool {
	for _, w := range want {
		if w == "*" || w == e.Type {
			return true
		}
	}
	return false
}
