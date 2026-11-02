package plugins

import (
	"strings"
	"testing"

	"github.com/pelican/wings/plugins/api"
)

// fixtureManifest and fixtureSource are a deliberately meaningless plugin.
//
// It exists to exercise the loader and the dispatch path, so it does the least
// a plugin can while still reaching each of them: it reads a setting, decides a
// file gate from it, and answers one route. It is shaped like nothing in
// particular on purpose, so these tests check how Wings loads and dispatches
// rather than how any real plugin happens to behave.
//
// The gate's three outcomes are driven by the "mode" setting, which is what
// lets one fixture cover allow, deny and handled without branching on anything
// domain-shaped.
const fixtureManifest = `{
    "id": "fixture",
    "name": "Fixture",
    "author": "Tests",
    "version": "1.0.0",
    "category": "plugin",
    "package": "fixture",
    "meta": {"status": "not_installed", "load_order": 0}
}`

const fixtureSource = `
package fixture

import (
	"github.com/pelican/wings/plugins/api"
)

type plugin struct {
	host api.Host
}

func (p *plugin) init(h api.Host) error {
	p.host = h
	return nil
}

func (p *plugin) beforeFile(e api.FileEvent) api.FileDirective {
	if e.Action != api.FileDelete {
		return api.AllowFile()
	}

	switch p.host.Settings().String("mode", "allow") {
	case "deny":
		return api.DenyFile("the mode setting says no")
	case "handle":
		// Taking the operation over means actually doing something, so the
		// test can tell a real handover from a claim of one.
		if err := p.host.Files().Write(e.Server.UUID, e.Path+".handled", []byte("handled")); err != nil {
			return api.DenyFile("could not record the handover: " + err.Error())
		}
		return api.FileHandled()
	}

	return api.AllowFile()
}

func (p *plugin) route(r api.Request) api.Response {
	return api.Text(200, "server:"+r.Server.Name)
}

func New() api.Registration {
	p := &plugin{}
	return api.Registration{
		Init:             p.init,
		BeforeFileAction: p.beforeFile,
		Settings: []api.Setting{
			{Key: "mode", Type: api.SettingString, Default: "allow"},
		},
		Routes: []api.Route{
			{Method: "GET", Path: "/", ServerScoped: true, Handler: p.route},
		},
	}
}
`

// enableTestPlugin writes a plugin, discovers it and installs it enabled.
func enableTestPlugin(t *testing.T, m *Manager, root, id, manifest, source string) *Instance {
	t.Helper()

	writePlugin(t, root, id, manifest, source)

	if err := m.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	inst, ok := m.Get(id)
	if !ok {
		t.Fatalf("plugin %q was not discovered", id)
	}
	if got := inst.Status(); got != StatusNotInstalled {
		t.Fatalf("a freshly discovered plugin should be not_installed, got %q", got)
	}

	if err := m.Install(id, true); err != nil {
		t.Fatalf("Install: %v (%s)", err, inst.Manifest().Meta.StatusMessage)
	}
	if got := inst.Status(); got != StatusEnabled {
		t.Fatalf("after installing with enable the status should be enabled, got %q (%s)",
			got, inst.Manifest().Meta.StatusMessage)
	}
	if !inst.Loaded() {
		t.Fatal("an enabled plugin should be loaded")
	}

	return inst
}

// enableFixture installs the fixture plugin with its gate in the given mode.
func enableFixture(t *testing.T, m *Manager, root, mode string) *Instance {
	t.Helper()

	inst := enableTestPlugin(t, m, root, "fixture", fixtureManifest, fixtureSource)

	if mode != "" {
		if err := m.SaveSettings("fixture", map[string]any{"mode": mode}); err != nil {
			t.Fatalf("SaveSettings: %v", err)
		}
	}
	return inst
}

func TestPluginLoadsFromSourceAndIsDispatchedTo(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)
	bridge.putFile(server.UUID, "/some/file.txt", []byte("contents"))

	m, root := newTestManager(t, bridge)
	enableFixture(t, m, root, "handle")

	allow, handled, reason := GateFileAction(api.FileEvent{
		Server: server,
		Action: api.FileDelete,
		Path:   "/some/file.txt",
		User:   "operator",
	})

	if !allow {
		t.Fatalf("the plugin did not deny, so the action should be allowed: %s", reason)
	}
	if !handled {
		t.Fatal("the plugin reported the action as handled, so Wings should skip its own work")
	}

	// Handing over means the plugin actually acted, not merely claimed to.
	if _, err := bridge.FileRead(server.UUID, "/some/file.txt.handled"); err != nil {
		t.Fatalf("the plugin should have acted through the host, paths are %v", bridge.paths(server.UUID))
	}
}

func TestGateAllowsWhenThePluginDoesNotObject(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)
	enableFixture(t, m, root, "allow")

	allow, handled, reason := GateFileAction(api.FileEvent{
		Server: server, Action: api.FileDelete, Path: "/some/file.txt",
	})

	if !allow || handled {
		t.Fatalf("expected a plain allow, got allow=%v handled=%v reason=%q", allow, handled, reason)
	}
}

func TestNonDeleteActionsPassStraightThrough(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)

	// Even with the gate set to refuse, the plugin only looks at deletes, so
	// everything else must be untouched.
	enableFixture(t, m, root, "deny")

	for _, action := range []api.FileAction{api.FileWrite, api.FileRename, api.FileChmod, api.FileRead} {
		allow, handled, reason := GateFileAction(api.FileEvent{
			Server: server, Action: action, Path: "/server.properties",
		})
		if !allow || handled {
			t.Errorf("%s should pass through untouched, got allow=%v handled=%v reason=%q",
				action, allow, handled, reason)
		}
	}
}

func TestGateDenialIsAttributedToThePlugin(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)
	enableFixture(t, m, root, "deny")

	allow, handled, reason := GateFileAction(api.FileEvent{
		Server: server, Action: api.FileDelete, Path: "/some/file.txt",
	})

	if allow {
		t.Fatal("the plugin denied the action, so it should not be allowed")
	}
	if handled {
		t.Fatal("a denied action is not a handled one")
	}
	// Whoever triggered the action needs to know which plugin stopped them.
	if !strings.HasPrefix(reason, "Fixture: ") {
		t.Fatalf("the denial should name the plugin, got %q", reason)
	}
}

func TestDisableStopsDispatchWithoutRestart(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)
	bridge.putFile(server.UUID, "/a.txt", []byte("a"))
	bridge.putFile(server.UUID, "/b.txt", []byte("b"))

	m, root := newTestManager(t, bridge)
	inst := enableFixture(t, m, root, "handle")

	if _, handled, _ := GateFileAction(api.FileEvent{
		Server: server, Action: api.FileDelete, Path: "/a.txt",
	}); !handled {
		t.Fatal("the enabled plugin should handle the action")
	}

	if err := m.Disable("fixture"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if got := inst.Status(); got != StatusDisabled {
		t.Fatalf("status after Disable should be disabled, got %q", got)
	}
	if inst.Loaded() {
		t.Fatal("a disabled plugin should not still be loaded")
	}

	// The whole point of interpreting plugins rather than linking them is that
	// this takes effect now, with no restart.
	if _, handled, _ := GateFileAction(api.FileEvent{
		Server: server, Action: api.FileDelete, Path: "/b.txt",
	}); handled {
		t.Fatal("a disabled plugin must not receive hooks any more")
	}

	if err := m.Enable("fixture"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, handled, _ := GateFileAction(api.FileEvent{
		Server: server, Action: api.FileDelete, Path: "/b.txt",
	}); !handled {
		t.Fatal("a re-enabled plugin should receive hooks again")
	}
}

func TestSettingsAreReadLiveWithoutReloading(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)
	bridge.putFile(server.UUID, "/a.txt", []byte("a"))

	m, root := newTestManager(t, bridge)
	enableFixture(t, m, root, "deny")

	if allow, _, _ := GateFileAction(api.FileEvent{
		Server: server, Action: api.FileDelete, Path: "/a.txt",
	}); allow {
		t.Fatal("the plugin should be refusing while the mode says so")
	}

	// Change the setting and the very next call behaves differently, with no
	// reload in between.
	if err := m.SaveSettings("fixture", map[string]any{"mode": "allow"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	if allow, handled, reason := GateFileAction(api.FileEvent{
		Server: server, Action: api.FileDelete, Path: "/a.txt",
	}); !allow || handled {
		t.Fatalf("the new setting should have taken effect, got allow=%v handled=%v reason=%q",
			allow, handled, reason)
	}
}

func TestSaveSettingsRejectsUndeclaredKeys(t *testing.T) {
	bridge := newFakeBridge()
	m, root := newTestManager(t, bridge)
	enableFixture(t, m, root, "")

	err := m.SaveSettings("fixture", map[string]any{"not_a_real_setting": 1})
	if err == nil {
		t.Fatal("saving a setting the plugin does not declare should fail")
	}
	if !strings.Contains(err.Error(), "not_a_real_setting") {
		t.Fatalf("the error should name the offending key, got %q", err)
	}
}

func TestServerScopedRouteIsResolvedAndInvoked(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)
	inst := enableFixture(t, m, root, "")

	found, route, _, ok := m.FindRoute("fixture", "GET", "/", true)
	if !ok {
		t.Fatal("the plugin's server scoped route should be resolvable")
	}
	if found != inst {
		t.Fatal("the route resolved to the wrong plugin")
	}

	res := route.Handler(api.Request{
		Method:       "GET",
		Path:         "/",
		Server:       server,
		ServerScoped: true,
	})
	if res.Status != 200 {
		t.Fatalf("the handler should have returned 200, got %d", res.Status)
	}
	if string(res.Body) != "server:Survival" {
		t.Fatalf("the handler got the wrong server, body was %q", res.Body)
	}

	// A node scoped lookup must not find a server scoped route.
	if _, _, _, ok := m.FindRoute("fixture", "GET", "/", false); ok {
		t.Fatal("a server scoped route should not answer a node scoped request")
	}

	// And a disabled plugin answers nothing.
	if err := m.Disable("fixture"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, _, _, ok := m.FindRoute("fixture", "GET", "/", true); ok {
		t.Fatal("a disabled plugin's routes should no longer resolve")
	}
}
