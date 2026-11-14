package plugin

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin/jsonrpc"
)

// HostAPI is the surface Wings exposes to plugins for host callbacks. A single
// Invoke entry point keeps this package decoupled from the server layer (which
// it must not import); the concrete implementation lives in a package that can
// reach the server manager. Adding host methods therefore never touches this
// file — only the implementation grows.
type HostAPI interface {
	// Invoke handles a host method identified by its grant key (e.g.
	// "server.stats") and returns a JSON-marshalable result. It is only ever
	// reached for a method the caller was already granted.
	Invoke(ctx context.Context, method string, params json.RawMessage) (any, error)
}

// HostHandler builds a jsonrpc.Handler dispatching inbound "host.*" calls from a
// plugin to api. The grant key is the method name minus the "host." prefix; the
// call runs only if that key is in the granted set. Unknown or ungranted methods
// return a JSON-RPC error, and a nil api rejects everything. This is the single
// mediated boundary through which every plugin -> host access flows.
func HostHandler(api HostAPI, granted []string) jsonrpc.Handler {
	allow := make(map[string]bool, len(granted))
	for _, g := range granted {
		allow[g] = true
	}
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		key, ok := strings.CutPrefix(method, "host.")
		if !ok {
			return nil, &jsonrpc.Error{Code: -32601, Message: "method not found"}
		}
		if api == nil || !allow[key] {
			return nil, &jsonrpc.Error{Code: -32001, Message: "capability not granted: " + key}
		}
		return api.Invoke(ctx, key, params)
	}
}

// GrantsFor computes the host capabilities effectively granted to a plugin: the
// manifest's requested capabilities narrowed by any per-plugin override in
// configuration policy. With no override the manifest request is honored as-is
// (the permissive first-party default); tightening this to user-approved grants
// is the M5 marketplace step.
func GrantsFor(m Manifest) []string {
	overrides := config.Get().Plugins.Grants
	if override, ok := overrides[m.Name]; ok {
		return intersect(m.Requires.HostAPI, override)
	}
	return m.Requires.HostAPI
}

func intersect(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, x := range b {
		set[x] = true
	}
	var out []string
	for _, x := range a {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}
