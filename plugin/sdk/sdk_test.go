package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/pelican/wings/plugin"
)

type recordingSink struct{ got chan plugin.Event }

func (recordingSink) Name() string     { return "audit" }
func (recordingSink) Events() []string { return []string{"server.state"} }
func (s recordingSink) OnEvent(_ context.Context, e plugin.Event) error {
	s.got <- e
	return nil
}

// TestServeEventSinkSpeaksTheWireProtocol drives the SDK the way Wings does:
// an initialize request, then an "event" notification, then stdin closes.
func TestServeEventSinkSpeaksTheWireProtocol(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	sink := recordingSink{got: make(chan plugin.Event, 1)}

	done := make(chan struct{})
	go func() {
		_ = serveEventSink(inR, outW, io.Discard, sink)
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
	case e := <-sink.got:
		if e.Type != "server.state" || e.Server != "abc" || e.Data["state"] != "running" {
			t.Fatalf("unexpected event: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event notification was not delivered to the sink")
	}

	_ = inW.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeEventSink did not return after stdin closed")
	}
}
