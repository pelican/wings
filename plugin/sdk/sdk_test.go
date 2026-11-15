package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pelican/wings/plugin"
)

type recordingListener struct{ got chan plugin.Event }

func (recordingListener) Name() string     { return "audit" }
func (recordingListener) Events() []string { return []string{"server.state"} }
func (s recordingListener) OnEvent(_ context.Context, e plugin.Event) error {
	s.got <- e
	return nil
}

// TestServeEventListenerSpeaksTheWireProtocol drives the SDK the way Wings does:
// an initialize request, then an "event" notification, then stdin closes.
func TestServeEventListenerSpeaksTheWireProtocol(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	listener := recordingListener{got: make(chan plugin.Event, 1)}

	done := make(chan struct{})
	go func() {
		_ = serveEventListener(inR, outW, io.Discard, listener)
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
func (s slowListener) OnEvent(context.Context, plugin.Event) error {
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
		_ = serveEventListener(inR, io.Discard, io.Discard, slowListener{handled: &handled})
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
		_ = serveEventListener(in, &out, io.Discard, recordingListener{got: make(chan plugin.Event, 1)})
		if !strings.Contains(out.String(), `"id":1`) {
			t.Fatalf("iteration %d: returned without answering initialize: %q", i, out.String())
		}
	}
}
