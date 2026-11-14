package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pelican/wings/plugin/jsonrpc"
	"github.com/pelican/wings/server"
)

// pluginHost implements plugin.HostAPI. It is the single mediated surface a
// plugin uses to reach Wings. Methods are registered into hostMethods from the
// domain files (pluginhost_server.go, pluginhost_fs.go, ...) so adding a method
// is one register() call rather than a new switch case, and each method stays a
// small typed function. Every server-scoped call resolves the target through the
// manager (a plugin can only act on a server that exists on this node) and
// returns curated projections — never the raw Configuration or container
// inspect, which carry per-tenant secrets.
type pluginHost struct {
	manager *server.Manager
}

// hostFn implements a single host method. When registered as scoped, the
// dispatcher resolves the {server} param and passes it in; otherwise s is nil.
type hostFn func(h pluginHost, ctx context.Context, s *server.Server, params json.RawMessage) (any, error)

type hostMethod struct {
	scoped bool
	fn     hostFn
}

// hostMethods is the dispatch registry, populated by the domain files via init().
var hostMethods = map[string]hostMethod{}

// register adds a host method to the registry. Duplicate registration is a
// programming error and panics at startup.
func register(method string, scoped bool, fn hostFn) {
	if _, dup := hostMethods[method]; dup {
		panic("plugin host: duplicate method " + method)
	}
	hostMethods[method] = hostMethod{scoped: scoped, fn: fn}
}

// Invoke dispatches a granted host method: resolve the server (if the method is
// scoped), then run the handler under panic recovery. Grant-gating already
// happened in plugin.HostHandler before Invoke is reached.
func (h pluginHost) Invoke(ctx context.Context, method string, params json.RawMessage) (res any, err error) {
	m, ok := hostMethods[method]
	if !ok {
		return nil, rpcErr(-32601, "method not found: "+method)
	}
	var s *server.Server
	if m.scoped {
		if s, err = h.scoped(params); err != nil {
			return nil, err
		}
	}
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, rpcErr(-32603, "host method panicked: "+method)
		}
	}()
	return m.fn(h, ctx, s, params)
}

// decode unmarshals params into T, mapping a failure to an invalid-params error.
func decode[T any](params json.RawMessage) (T, error) {
	var t T
	if err := json.Unmarshal(params, &t); err != nil {
		return t, rpcErr(-32602, "invalid params")
	}
	return t, nil
}

func rpcErr(code int, msg string) error { return &jsonrpc.Error{Code: code, Message: msg} }

// okResult is the standard acknowledgement for mutating methods with no payload.
var okResult = map[string]bool{"ok": true}

// serverParam is the minimal shape every server-scoped method carries.
type serverParam struct {
	Server string `json:"server"`
}

// get resolves a server UUID to a server on this node (the scope gate) or a
// JSON-RPC error.
func (h pluginHost) get(id string) (*server.Server, error) {
	s, ok := h.manager.Get(id)
	if !ok {
		return nil, rpcErr(-32004, fmt.Sprintf("unknown server %q", id))
	}
	return s, nil
}

// scoped resolves the {server} param — the gate every server-bound method passes.
func (h pluginHost) scoped(params json.RawMessage) (*server.Server, error) {
	p, err := decode[serverParam](params)
	if err != nil {
		return nil, err
	}
	return h.get(p.Server)
}

// --- shared projections (no secrets, no mutex) ---

type serverInfo struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Suspended bool   `json:"suspended"`
}

type serverStatus struct {
	Running      bool `json:"running"`
	Suspended    bool `json:"suspended"`
	Installing   bool `json:"installing"`
	Transferring bool `json:"transferring"`
	Restoring    bool `json:"restoring"`
	Protected    bool `json:"protected"`
}

type allocationsView struct {
	Default  address          `json:"default"`
	Mappings map[string][]int `json:"mappings"`
}

type address struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// defaultAddress returns the server's default allocation, or a zero address
// when the Panel has not sent one.
func defaultAddress(s *server.Server) address {
	m := s.Config().Allocations.DefaultMapping
	if m == nil {
		return address{}
	}
	return address{IP: m.Ip, Port: m.Port}
}

func (h pluginHost) list() []serverInfo {
	all := h.manager.All()
	out := make([]serverInfo, 0, len(all))
	for _, s := range all {
		out = append(out, h.info(s))
	}
	return out
}

// info is the public projection of a server — no env vars, mounts, or egg
// internals.
func (h pluginHost) info(s *server.Server) serverInfo {
	return serverInfo{
		UUID:      s.ID(),
		Name:      s.Config().Meta.Name,
		State:     s.Environment.State(),
		Suspended: s.IsSuspended(),
	}
}
