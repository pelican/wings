// Package sdk helps authors write Wings plugins in Go. A plugin is an ordinary
// executable that calls one of the Serve functions and speaks JSON-RPC over its
// stdio; the same wire protocol is documented in plugin/sdk/SPEC.md so plugins
// can equally be written in Node, Rust, Python, or anything else.
package sdk

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/pelican/wings/plugin"
	"github.com/pelican/wings/plugin/jsonrpc"
)

// ServeEventListener runs an events-capability plugin, blocking until the host
// closes stdin. Each "event" notification from Wings is passed to s.OnEvent;
// an error returned from OnEvent is written to stderr, which Wings logs.
func ServeEventListener(s plugin.EventListener) error {
	return serveEventListener(os.Stdin, os.Stdout, os.Stderr, s)
}

func serveEventListener(r io.Reader, w io.Writer, errw io.Writer, s plugin.EventListener) error {
	h := eventListenerHandler{s: s, errw: errw}
	conn := jsonrpc.NewConn(r, w, h.handle, nil)
	<-conn.Done()
	return nil
}

type eventListenerHandler struct {
	s    plugin.EventListener
	errw io.Writer
}

func (h eventListenerHandler) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{
			"name":       h.s.Name(),
			"capability": string(plugin.CapabilityEvents),
			"provides":   map[string]any{"events": h.s.Events()},
		}, nil
	case "event":
		var e plugin.Event
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
