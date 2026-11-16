package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type recordingListener struct{ got chan Event }

func (recordingListener) Name() string     { return "audit" }
func (recordingListener) Events() []string { return []string{"server.state"} }
func (s recordingListener) OnEvent(_ context.Context, e Event) error {
	s.got <- e
	return nil
}

// TestServeEventListenerSpeaksTheWireProtocol drives the SDK the way Wings does:
// an initialize request, then an "event" notification, then stdin closes.
func TestServeEventListenerSpeaksTheWireProtocol(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	listener := recordingListener{got: make(chan Event, 1)}

	done := make(chan struct{})
	go func() {
		_ = serveListener(inR, outW, io.Discard, listener)
		close(done)
	}()

	write := func(s string) {
		t.Helper()
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	write(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"host":"wings","version":1}}`)
	line, err := bufio.NewReader(outR).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read initialize response: %v", err)
	}
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			Name       string `json:"name"`
			Capability string `json:"capability"`
			Provides   struct {
				Events []string `json:"events"`
			} `json:"provides"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	if resp.ID != 1 || resp.Result.Name != "audit" || resp.Result.Capability != "events" ||
		len(resp.Result.Provides.Events) != 1 || resp.Result.Provides.Events[0] != "server.state" {
		t.Fatalf("unexpected initialize response: %s", line)
	}

	write(`{"jsonrpc":"2.0","method":"event","params":{"type":"server.state","server":"abc","data":{"state":"running"}}}`)
	select {
	case e := <-listener.got:
		if e.Type != "server.state" || e.Server != "abc" || e.Data["state"] != "running" {
			t.Fatalf("unexpected event: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event notification was not delivered to the listener")
	}

	_ = inW.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeEventListener did not return after stdin closed")
	}
}

type slowListener struct{ handled *atomic.Int32 }

func (slowListener) Name() string     { return "slow" }
func (slowListener) Events() []string { return []string{"*"} }
func (s slowListener) OnEvent(context.Context, Event) error {
	time.Sleep(5 * time.Millisecond)
	s.handled.Add(1)
	return nil
}

// TestServeEventListenerHandlesQueuedEventsBeforeReturning covers Wings
// closing stdin right after sending events: the plugin usually exits as soon
// as ServeEventListener returns, so it must not return with events queued.
func TestServeEventListenerHandlesQueuedEventsBeforeReturning(t *testing.T) {
	var handled atomic.Int32
	inR, inW := io.Pipe()
	done := make(chan struct{})
	go func() {
		_ = serveListener(inR, io.Discard, io.Discard, slowListener{handled: &handled})
		close(done)
	}()
	for i := 0; i < 20; i++ {
		if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","method":"event","params":{"type":"server.state"}}`+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	_ = inW.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeEventListener did not return")
	}
	if n := handled.Load(); n != 20 {
		t.Fatalf("returned after handling %d of 20 events", n)
	}
}

// TestServeEventListenerAnswersBeforeReturning covers a host that closes stdin
// right after a request: the plugin used to return, and usually exit, before
// writing the response.
func TestServeEventListenerAnswersBeforeReturning(t *testing.T) {
	for i := 0; i < 50; i++ {
		var out bytes.Buffer
		in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"host":"wings","version":1}}` + "\n")
		_ = serveListener(in, &out, io.Discard, recordingListener{got: make(chan Event, 1)})
		if !strings.Contains(out.String(), `"id":1`) {
			t.Fatalf("iteration %d: returned without answering initialize: %q", i, out.String())
		}
	}
}

// serveListener runs serve for a listener that doesn't use the Host.
func serveListener(r io.Reader, w io.Writer, errw io.Writer, s EventListener) error {
	return serve(r, w, errw, func(*Host) EventListener { return s })
}

// logsListener asks Wings for console lines on every event.
type logsListener struct {
	host *Host
	got  chan []string
	errs chan error
}

func (logsListener) Name() string     { return "logs" }
func (logsListener) Events() []string { return []string{"*"} }
func (l logsListener) OnEvent(ctx context.Context, e Event) error {
	var res struct {
		Lines []string `json:"lines"`
	}
	if err := l.host.Call(ctx, "server.logs", map[string]any{"server": e.Server}, &res); err != nil {
		l.errs <- err
		return err
	}
	l.got <- res.Lines
	return nil
}

// TestServeGivesTheListenerAHost plays Wings: it sends an event, answers the
// host call the listener makes while handling it, and checks the listener
// gets the answer. A refused call must come back as an *Error.
func TestServeGivesTheListenerAHost(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	l := logsListener{got: make(chan []string, 1), errs: make(chan error, 1)}
	go func() {
		_ = serve(inR, outW, io.Discard, func(h *Host) EventListener { l.host = h; return l })
	}()
	t.Cleanup(func() { _ = inW.Close() })
	out := bufio.NewReader(outR)

	readCall := func() (id json.RawMessage, method string, params map[string]any) {
		t.Helper()
		line, err := out.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read host call: %v", err)
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
		return m.ID, m.Method, m.Params
	}

	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","method":"event","params":{"type":"server.state","server":"abc"}}`+"\n")
	id, method, params := readCall()
	if method != "host.server.logs" || params["server"] != "abc" {
		t.Fatalf("host call = %s %v, want host.server.logs for abc", method, params)
	}
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":`+string(id)+`,"result":{"lines":["stopped"]}}`+"\n")
	select {
	case lines := <-l.got:
		if len(lines) != 1 || lines[0] != "stopped" {
			t.Fatalf("lines = %q", lines)
		}
	case err := <-l.errs:
		t.Fatalf("Call: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never got the host's answer")
	}

	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","method":"event","params":{"type":"server.state","server":"abc"}}`+"\n")
	id, _, _ = readCall()
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":`+string(id)+`,"error":{"code":-32001,"message":"capability not granted: server.logs"}}`+"\n")
	select {
	case err := <-l.errs:
		var rpcErr *Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32001 {
			t.Fatalf("Call error = %v, want *Error with code -32001", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the refused call never returned")
	}
}
