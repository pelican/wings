package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
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

// TestStringAndNumberIDsAreEchoed covers a peer that is not this package — for
// example a JavaScript plugin — issuing requests with string ids, which JSON-RPC
// 2.0 allows. The id must be echoed back as sent and the connection must stay
// up; a numeric-only id once made the decoder fail and drop the plugin.
func TestStringAndNumberIDsAreEchoed(t *testing.T) {
	hostR, peerW := io.Pipe()
	peerR, hostW := io.Pipe()
	host := NewConn(hostR, hostW, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		return map[string]string{"method": method}, nil
	}, nil)
	t.Cleanup(func() { _ = host.Close() })

	dec := json.NewDecoder(peerR)
	for _, id := range []string{`"p1"`, `7`, `"p2"`} {
		req := `{"jsonrpc":"2.0","id":` + id + `,"method":"host.ping"}` + "\n"
		if _, err := io.WriteString(peerW, req); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result struct{ Method string }
			Error  *Error
		}
		decoded := make(chan error, 1)
		go func() { decoded <- dec.Decode(&resp) }()
		select {
		case err := <-decoded:
			if err != nil {
				t.Fatalf("decode response to id %s: %v", id, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no response to id %s; the connection was dropped", id)
		}
		if string(resp.ID) != id || resp.Error != nil || resp.Result.Method != "host.ping" {
			t.Fatalf("id %s: got id=%s result=%+v error=%v", id, resp.ID, resp.Result, resp.Error)
		}
	}
	select {
	case <-host.Done():
		t.Fatal("connection closed after a string id")
	default:
	}
}

// TestWriteTimesOutWhenThePeerStopsReading covers a plugin that stops reading
// its stdin: once the pipe is full every write used to block forever.
func TestWriteTimesOutWhenThePeerStopsReading(t *testing.T) {
	prev := WriteTimeout
	WriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { WriteTimeout = prev })

	hostR, _ := io.Pipe() // nothing ever writes to the host
	_, hostW := io.Pipe() // nothing ever reads what the host writes
	host := NewConn(hostR, hostW, nil, nil)
	WriteTimeout = prev // the connection has its own copy now

	start := time.Now()
	err := host.Notify("event", map[string]string{"type": "server.state"})
	if !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("Notify = %v, want ErrWriteTimeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Notify took %v", d)
	}
	select {
	case <-host.Done():
	default:
		t.Fatal("the connection should be closed after a write timeout")
	}
	if err := host.Notify("event", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("a later write = %v, want ErrClosed", err)
	}
}
