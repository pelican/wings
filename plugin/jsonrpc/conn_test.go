package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strconv"
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

// writeNotifications sends n "event" notifications numbered 0..n-1.
func writeNotifications(t *testing.T, w io.Writer, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		msg := `{"jsonrpc":"2.0","method":"event","params":{"n":` + strconv.Itoa(i) + `}}` + "\n"
		if _, err := io.WriteString(w, msg); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func TestNotificationsAreHandledInOrder(t *testing.T) {
	got := make(chan int, 100)
	r, w := io.Pipe()
	c := NewConn(r, io.Discard, func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var p struct{ N int }
		_ = json.Unmarshal(params, &p)
		time.Sleep(time.Duration(p.N%3) * time.Millisecond) // uneven work per event
		got <- p.N
		return nil, nil
	}, nil)
	t.Cleanup(func() { _ = c.Close() })

	writeNotifications(t, w, 50)
	for want := 0; want < 50; want++ {
		select {
		case n := <-got:
			if n != want {
				t.Fatalf("notification %d handled at position %d", n, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("notification %d never handled", want)
		}
	}
}

func TestSlowNotificationHandlerStaysBounded(t *testing.T) {
	prev := NotificationQueueSize
	NotificationQueueSize = 8
	release := make(chan struct{})
	handled := make(chan int, 1000)
	r, w := io.Pipe()
	c := NewConn(r, io.Discard, func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var p struct{ N int }
		_ = json.Unmarshal(params, &p)
		<-release
		handled <- p.N
		return nil, nil
	}, nil)
	NotificationQueueSize = prev
	t.Cleanup(func() { _ = c.Close() })

	before := runtime.NumGoroutine()
	writeNotifications(t, w, 1000)
	if grew := runtime.NumGoroutine() - before; grew > 5 {
		t.Fatalf("a stuck handler made the connection start %d goroutines", grew)
	}
	close(release)

	// Roughly one in the handler plus a full queue get through, in order; the
	// rest are dropped. A few of the last notifications may still be in the
	// decoder's buffer when the handler is released, so the exact split varies.
	var got []int
	deadline := time.After(5 * time.Second)
	for len(got)+int(c.Dropped()) < 1000 {
		select {
		case n := <-handled:
			got = append(got, n)
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("handled %d and dropped %d of 1000", len(got), c.Dropped())
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("handled out of order: %v", got)
		}
	}
	if len(got) > 20 {
		t.Fatalf("%d notifications got through a queue of 8 while the handler was stuck", len(got))
	}
}

// TestNotificationHandlerCanCallBack covers a plugin that asks Wings for
// something while handling an event; that needs the read loop to keep running.
func TestNotificationHandlerCanCallBack(t *testing.T) {
	hostR, peerW := io.Pipe()
	peerR, hostW := io.Pipe()

	answered := make(chan string, 1)
	var peer *Conn
	ready := make(chan struct{})
	peer = NewConn(peerR, peerW, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		<-ready
		var out struct{ Lines []string }
		if err := peer.Call(ctx, "host.server.logs", nil, &out); err != nil {
			answered <- "error: " + err.Error()
			return nil, nil
		}
		answered <- out.Lines[0]
		return nil, nil
	}, nil)
	close(ready)
	host := NewConn(hostR, hostW, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		return map[string][]string{"lines": {"server stopped"}}, nil
	}, nil)
	t.Cleanup(func() { _ = host.Close(); _ = peer.Close() })

	if err := host.Notify("event", map[string]string{"type": "server.state"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-answered:
		if got != "server stopped" {
			t.Fatalf("host call from a notification handler: %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a host call made while handling a notification never returned")
	}
}

// TestCallReturnsAResponseSentJustBeforeClose covers a peer that answers and
// then hangs up at once. The read loop hands the response over before it
// closes the connection, so Call must return it rather than ErrClosed. The
// two arrive almost together, so the case is repeated to catch the race.
func TestCallReturnsAResponseSentJustBeforeClose(t *testing.T) {
	for i := 0; i < 500; i++ {
		hostR, peerW := io.Pipe()
		peerR, hostW := io.Pipe()
		host := NewConn(hostR, hostW, nil, nil)

		go func() {
			var req struct {
				ID json.RawMessage `json:"id"`
			}
			if err := json.NewDecoder(peerR).Decode(&req); err != nil {
				return
			}
			_, _ = io.WriteString(peerW, `{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":{"ok":true}}`+"\n")
			_ = peerW.Close()
		}()

		var out struct{ OK bool }
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := host.Call(ctx, "ping", nil, &out)
		cancel()
		_ = host.Close()
		_ = peerR.Close()
		if err != nil || !out.OK {
			t.Fatalf("iteration %d: Call = %v, ok=%v; the answered call was reported as failed", i, err, out.OK)
		}
	}
}

// TestRequestFloodStaysBounded covers a peer that sends requests faster than
// they are handled: each one used to get its own goroutine.
func TestRequestFloodStaysBounded(t *testing.T) {
	prev := MaxConcurrentRequests
	MaxConcurrentRequests = 4
	release := make(chan struct{})
	hostR, peerW := io.Pipe()
	peerR, hostW := io.Pipe()
	host := NewConn(hostR, hostW, func(context.Context, string, json.RawMessage) (any, error) {
		<-release
		return "done", nil
	}, nil)
	MaxConcurrentRequests = prev
	t.Cleanup(func() { _ = host.Close() })

	type reply struct {
		ID    int
		Error *Error
	}
	replies := make(chan reply, 200)
	go func() {
		dec := json.NewDecoder(peerR)
		for {
			var r reply
			if err := dec.Decode(&r); err != nil {
				return
			}
			replies <- r
		}
	}()

	before := runtime.NumGoroutine()
	for i := 1; i <= 100; i++ {
		msg := `{"jsonrpc":"2.0","id":` + strconv.Itoa(i) + `,"method":"host.server.list"}` + "\n"
		if _, err := io.WriteString(peerW, msg); err != nil {
			t.Fatal(err)
		}
	}
	// 96 refusals arrive while the 4 accepted requests are still blocked.
	busy := 0
	deadline := time.After(5 * time.Second)
	for busy < 96 {
		select {
		case r := <-replies:
			if r.Error == nil || r.Error.Code != ErrCodeBusy {
				t.Fatalf("reply %d: want a busy error while 4 requests are in flight, got %+v", r.ID, r.Error)
			}
			busy++
		case <-deadline:
			t.Fatalf("got %d busy replies, want 96", busy)
		}
	}
	if grew := runtime.NumGoroutine() - before; grew > 4+5 {
		t.Fatalf("100 requests started %d goroutines with a limit of 4", grew)
	}

	close(release)
	for done := 0; done < 4; {
		select {
		case r := <-replies:
			if r.Error != nil {
				t.Fatalf("accepted request %d failed: %+v", r.ID, r.Error)
			}
			done++
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of the 4 accepted requests finished", done)
		}
	}
}
