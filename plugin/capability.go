// Package plugin implements Wings' capability-provider plugin system. A
// capability (for example "events") is a typed contract; providers register
// against a capability and the host dispatches to every enabled provider for
// it. Providers may be native (compiled into Wings and registered from an
// init) or external processes reached over a transport — the host treats both
// identically.
package plugin

// Capability identifies a category of plugin functionality. New capabilities
// are added as a typed Go interface plus a constant here; the registry,
// manifest discovery, trust and transport layers do not change.
type Capability string
