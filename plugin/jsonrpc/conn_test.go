package jsonrpc

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// pair wires two Conns together over in-memory pipes and returns the "host" side
// plus a cleanup. The peer uses the supplied handler.
func pair(t *testing.T, peerHandler, hostHandler Handler) *Conn {
	t.Helper()
	hostR, peerW := io.Pipe()
	peerR, hostW := io.Pipe()
	_ = NewConn(peerR, peerW, peerHandler, nil)
	host := NewConn(hostR, hostW, hostHandler, nil)
	t.Cleanup(func() { _ = host.Close() })
	return host
}

func TestCallRoundTrip(t *testing.T) {
	peer := func(_ context.Context, method string, params json.RawMessage) (any, error) {
		if method != "add" {
			return nil, &Error{Code: -32601, Message: "method not found"}
		}
		var in struct{ A, B int }
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &Error{Code: -32602, Message: "invalid params"}
		}
		return map[string]int{"sum": in.A + in.B}, nil
	}
	host := pair(t, peer, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var out struct{ Sum int }
	if err := host.Call(ctx, "add", map[string]int{"A": 2, "B": 3}, &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.Sum != 5 {
		t.Fatalf("expected sum 5, got %d", out.Sum)
	}
}

func TestCallPropagatesError(t *testing.T) {
	peer := func(context.Context, string, json.RawMessage) (any, error) {
		return nil, &Error{Code: -32000, Message: "boom"}
	}
	host := pair(t, peer, nil)

	err := host.Call(context.Background(), "anything", nil, nil)
	var e *Error
	if err == nil {
		t.Fatal("expected an error")
	}
	if !asError(err, &e) || e.Message != "boom" {
		t.Fatalf("expected structured error boom, got %v", err)
	}
}

func TestBidirectionalHostCallback(t *testing.T) {
	// The peer, while handling "go", calls back into the host's "host.ping".
	done := make(chan string, 1)
	hostHandler := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "host.ping" {
			return "pong", nil
		}
		return nil, &Error{Code: -32601, Message: "nf"}
	}

	var peerConn *Conn
	peer := func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "go" {
			var reply string
			if err := peerConn.Call(ctx, "host.ping", nil, &reply); err != nil {
				return nil, err
			}
			done <- reply
			return map[string]bool{"ok": true}, nil
		}
		return nil, &Error{Code: -32601, Message: "nf"}
	}

	// Build both sides manually so the peer can call back.
	hostR, peerW := io.Pipe()
	peerR, hostW := io.Pipe()
	peerConn = NewConn(peerR, peerW, peer, nil)
	host := NewConn(hostR, hostW, hostHandler, nil)
	t.Cleanup(func() { _ = host.Close(); _ = peerConn.Close() })

	if err := host.Call(context.Background(), "go", nil, nil); err != nil {
		t.Fatalf("call go: %v", err)
	}
	select {
	case got := <-done:
		if got != "pong" {
			t.Fatalf("expected pong, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for host callback")
	}
}

func TestCallAfterCloseFails(t *testing.T) {
	host := pair(t, nil, nil)
	_ = host.Close()
	if err := host.Call(context.Background(), "x", nil, nil); err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

func TestHandlerPanicBecomesError(t *testing.T) {
	peer := func(context.Context, string, json.RawMessage) (any, error) {
		panic("boom")
	}
	host := pair(t, peer, nil)

	if err := host.Call(context.Background(), "explode", nil, nil); err == nil {
		t.Fatal("expected an error from a panicking handler")
	}
	// The connection must survive the panic — a follow-up call still returns.
	if err := host.Call(context.Background(), "explode", nil, nil); err == nil {
		t.Fatal("expected the connection to survive the handler panic")
	}
}

// asError is a tiny errors.As shim kept local to avoid importing errors in tests
// merely for the assertion helper.
func asError(err error, target **Error) bool {
	if e, ok := err.(*Error); ok {
		*target = e
		return true
	}
	return false
}
