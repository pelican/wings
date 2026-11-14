// Package builtin is the one place native (compiled-in) plugins are linked into
// Wings. Each native plugin is an ordinary Go package that registers itself
// from init() via plugin.RegisterEventListener, plugin.RegisterRoutes or
// plugin.RegisterTask; importing it here with the blank identifier is all it
// takes to ship it.
//
// Wings itself ships no native plugins yet. A fork that adds its own keeps them
// in its own packages and adds one import line below, so carrying them across
// upstream merges never touches router, server or cmd code:
//
//	import (
//	    _ "example.com/my-fork/wings-plugins/auditlog"
//	)
package builtin
