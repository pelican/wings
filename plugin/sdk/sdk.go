// Package sdk helps authors write Wings plugins in Go. A plugin is an ordinary
// executable that calls Serve (or ServeEventListener) and speaks JSON-RPC over
// its stdio; the same wire protocol is documented in plugin/sdk/SPEC.md so
// plugins can equally be written in Node, Rust, Python, or anything else.
//
// The package imports only the standard library, plugin/contract and
// plugin/jsonrpc, so a plugin built with it does not pull in Wings.
package sdk

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/pelican/wings/plugin/contract"
	"github.com/pelican/wings/plugin/jsonrpc"
)

// Event is a server lifecycle notification from Wings.
type Event = contract.Event

// EventListener is what an events plugin implements.
type EventListener = contract.EventListener

// Error is the error a refused host call returns. Code -32001 means the
// method is not granted to this plugin; -32601 means Wings has no such method.
type Error = jsonrpc.Error

// Host calls back into Wings. Only the methods listed under requires.hostAPI
// in plugin.yaml, and approved by the operator, are allowed.
type Host struct {
	conn *jsonrpc.Conn
}

// Call invokes a host method, for example "server.logs" (the "host." prefix
// is added if missing), and decodes its result into result, which may be nil.
// It is safe to call from OnEvent and from several goroutines.
func (h *Host) Call(ctx context.Context, method string, params, result any) error {
	if !strings.HasPrefix(method, "host.") {
		method = "host." + method
	}
	return h.conn.Call(ctx, method, params, result)
}

// Serve runs an events plugin, blocking until Wings closes stdin. newListener
// is called once, before any event, with the Host the listener uses to call
// back into Wings. Each "event" notification is passed to OnEvent one at a
// time, in the order Wings sent them; if OnEvent falls
// jsonrpc.NotificationQueueSize events behind, newer ones are dropped. An
// error returned from OnEvent is written to stderr, which Wings logs.
func Serve(newListener func(*Host) EventListener) error {
	return serve(os.Stdin, os.Stdout, os.Stderr, newListener)
}

// ServeEventListener is Serve for a listener that never calls back into Wings.
func ServeEventListener(s EventListener) error {
	return Serve(func(*Host) EventListener { return s })
}

func serve(r io.Reader, w io.Writer, errw io.Writer, newListener func(*Host) EventListener) error {
	host := &Host{}
	h := &handler{s: newListener(host), errw: errw, ready: make(chan struct{})}
	host.conn = jsonrpc.NewConn(r, w, h.handle, nil)
	close(h.ready) // host.conn is set; handlers may now use it
	// Return only after the events Wings sent before closing stdin have all
	// reached OnEvent; the plugin usually exits right after this returns.
	<-host.conn.Drained()
	return nil
}

type handler struct {
	s     EventListener
	errw  io.Writer
	ready chan struct{}
}

func (h *handler) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	<-h.ready
	switch method {
	case "initialize":
		return map[string]any{
			"name":       h.s.Name(),
			"capability": string(contract.CapabilityEvents),
			"provides":   map[string]any{"events": h.s.Events()},
		}, nil
	case "event":
		var e Event
		if err := json.Unmarshal(params, &e); err != nil {
			return nil, &jsonrpc.Error{Code: -32602, Message: "invalid params"}
		}
		if err := h.s.OnEvent(ctx, e); err != nil {
			_, _ = io.WriteString(h.errw, "event handler: "+err.Error()+"\n")
		}
		return nil, nil
	default:
		return nil, &jsonrpc.Error{Code: -32601, Message: "method not found"}
	}
}
