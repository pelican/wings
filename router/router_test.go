package router

import (
	"testing"

	"github.com/pelican/wings/server"
)

// TestConfigureDoesNotPanic builds the whole route tree.
//
// Gin validates its radix tree as routes are added and panics on a conflict,
// such as a catch-all registered beside static siblings. Nothing else catches
// that: the package compiles, every unit test passes, and the daemon then dies
// on boot before it serves a single request. Building the tree here turns that
// into a test failure instead of a failed deploy.
func TestConfigureDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("building the router panicked, so Wings would not start: %v", r)
		}
	}()

	m := &server.Manager{}

	if engine := Configure(m, nil); engine == nil {
		t.Fatal("Configure returned no engine")
	}
}

// TestPluginRoutesAreRegistered checks that the plugin endpoints are actually
// in the tree, including the catch-alls that carry plugin-registered routes.
func TestPluginRoutesAreRegistered(t *testing.T) {
	engine := Configure(&server.Manager{}, nil)

	got := map[string]bool{}
	for _, r := range engine.Routes() {
		got[r.Method+" "+r.Path] = true
	}

	for _, want := range []string{
		"GET /api/plugins",
		"GET /api/plugins/:plugin",
		"POST /api/plugins/:plugin/enable",
		"POST /api/plugins/:plugin/disable",
		"POST /api/plugins/:plugin/uninstall",
		"POST /api/plugins/:plugin/install",
		"PUT /api/plugins/:plugin/settings",
		// The mount for routes plugins register for themselves, in both
		// scopes. GET stands in for the whole Any() set.
		"GET /api/plugins/:plugin/http/*path",
		"GET /api/servers/:server/plugins/:plugin/http/*path",
	} {
		if !got[want] {
			t.Errorf("route %q is not registered", want)
		}
	}
}
