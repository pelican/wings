package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin/jsonrpc"
)

// fakeHost records the last invoked method and returns the configured address
// for "resolveAddress".
type fakeHost struct {
	addr   string
	called string
}

func (f *fakeHost) Invoke(_ context.Context, method string, _ json.RawMessage) (any, error) {
	f.called = method
	if method == "resolveAddress" {
		return map[string]string{"address": f.addr}, nil
	}
	return nil, &jsonrpc.Error{Code: -32601, Message: "method not found"}
}

func TestHostHandlerGrantedCall(t *testing.T) {
	api := &fakeHost{addr: "1.2.3.4:5"}
	h := HostHandler(api, []string{"resolveAddress"})

	res, err := h(context.Background(), "host.resolveAddress", json.RawMessage(`{"server":"abc"}`))
	if err != nil {
		t.Fatalf("granted call failed: %v", err)
	}
	if api.called != "resolveAddress" {
		t.Fatalf("expected grant-key %q reached Invoke, got %q", "resolveAddress", api.called)
	}
	if m, ok := res.(map[string]string); !ok || m["address"] != "1.2.3.4:5" {
		t.Fatalf("unexpected result: %v", res)
	}
}

func TestHostHandlerDeniesUngranted(t *testing.T) {
	api := &fakeHost{}
	h := HostHandler(api, nil) // no grants

	if _, err := h(context.Background(), "host.resolveAddress", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected capability-not-granted error")
	}
	if api.called != "" {
		t.Fatal("Invoke must not run for an ungranted method")
	}
}

func TestHostHandlerNilAPIDenies(t *testing.T) {
	h := HostHandler(nil, []string{"resolveAddress"})
	if _, err := h(context.Background(), "host.resolveAddress", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected denial when api is nil")
	}
}

func TestHostHandlerRejectsNonHostMethod(t *testing.T) {
	h := HostHandler(&fakeHost{}, []string{"resolveAddress"})
	// Missing the "host." prefix must not be dispatched.
	if _, err := h(context.Background(), "resolveAddress", nil); err == nil {
		t.Fatal("expected method-not-found for a non-host method")
	}
}

func TestGrantsForOverrideIntersects(t *testing.T) {
	c := setConfig(t)
	c.Plugins.Grants = map[string][]string{"p": {"server.stats"}}
	config.Set(c)

	m := Manifest{Name: "p", Requires: Requires{HostAPI: []string{"server.stats", "server.power"}}}
	g := GrantsFor(m)
	if len(g) != 1 || g[0] != "server.stats" {
		t.Fatalf("expected intersection [server.stats], got %v", g)
	}
}

func TestGrantsForNoOverrideIsPermissive(t *testing.T) {
	setConfig(t) // no grants override
	m := Manifest{Name: "p", Requires: Requires{HostAPI: []string{"server.stats"}}}
	if g := GrantsFor(m); len(g) != 1 || g[0] != "server.stats" {
		t.Fatalf("expected manifest request honored, got %v", g)
	}
}
