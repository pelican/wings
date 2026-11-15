// Package api defines the contract between Wings and a Wings plugin.
//
// A plugin is distributed as readable Go source, not as a compiled artifact, so
// that a node operator can audit exactly what a plugin does before enabling it.
// Wings loads that source with an embedded Go interpreter, which means a plugin
// needs no build step, no toolchain on the node, and can be enabled, disabled
// and uninstalled while Wings keeps running.
//
// # Writing a plugin
//
// A plugin is a directory under the configured plugin directory containing a
// plugin.json manifest and at least one .go file. Every file in the directory
// must belong to the same package, and that package must export a New function
// returning a [Registration]:
//
//	package maintenance
//
//	import "github.com/pelican/wings/plugins/api"
//
//	type plugin struct {
//		host api.Host
//	}
//
//	func (p *plugin) init(h api.Host) error {
//		p.host = h
//		h.Logger().Info("maintenance window plugin ready")
//		return nil
//	}
//
//	func (p *plugin) beforeStart(s api.Server) api.Decision {
//		if p.inWindow() {
//			return api.Deny("node is in a maintenance window")
//		}
//		return api.Allow()
//	}
//
//	func New() api.Registration {
//		p := &plugin{}
//		return api.Registration{
//			Init:              p.init,
//			BeforeServerStart: p.beforeStart,
//		}
//	}
//
// Every field on [Registration] is optional. Leave a hook nil and Wings will
// never call it, which also means it costs nothing. Wings recovers from panics
// raised inside a hook: the hook is treated as having made no decision, the
// plugin is marked errored, and the daemon carries on.
//
// # What a plugin can reach
//
// Hooks receive plain value snapshots ([Server], [ConsoleLine], [FileEvent] and
// friends) rather than live Wings internals, so a plugin cannot corrupt daemon
// state by mutating what it was handed. To act on the node a plugin calls back
// through [Host], which it receives once in Init.
//
// # Versioning
//
// [Version] is the contract version. A manifest declaring a higher api_version
// than the running Wings supports is refused rather than loaded, the same way
// the Panel refuses a plugin built for a newer Panel.
package api
