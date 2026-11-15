package plugins

import (
	"testing"
	"time"

	"github.com/pelican/wings/plugins/api"
)

const interfacesManifest = `{
    "id": "watcher",
    "name": "Watcher",
    "author": "Tests",
    "version": "1.0.0",
    "category": "plugin",
    "package": "watcher",
    "meta": {"status": "not_installed", "load_order": 0}
}`

// interfacesSource exercises the interfaces added for running a command in a
// container, reacting to a server being reconfigured, and watching console
// connections come and go.
const interfacesSource = `
package watcher

import (
	"strconv"
	"strings"

	"github.com/pelican/wings/plugins/api"
)

type plugin struct {
	host api.Host

	reconfigured int
	opened       int
	closed       int
	lastViewer   string
}

func (p *plugin) init(h api.Host) error {
	p.host = h
	return nil
}

func (p *plugin) lifecycle(l api.Lifecycle) {
	if l.Event == api.ServerReconfigured {
		p.reconfigured++
	}
}

func (p *plugin) opened_(c api.WebsocketConnection) {
	p.opened++
	p.lastViewer = c.User
}

func (p *plugin) closed_(c api.WebsocketConnection) {
	p.closed++
}

// health runs a command in the container and reports what it said, which is
// the thing that was impossible before: the console can only be written to.
func (p *plugin) health(r api.Request) api.Response {
	result, err := p.host.Servers().Exec(r.Server.UUID, []string{"sh", "-c", "echo ok"}, api.ExecOptions{
		TimeoutSeconds: 5,
	})
	if err != nil {
		return api.Text(500, "exec failed: "+err.Error())
	}

	return api.Text(200, strings.TrimSpace(result.Stdout)+" exit="+strconv.Itoa(result.ExitCode))
}

func (p *plugin) diagnostics() map[string]string {
	return map[string]string{
		"reconfigured": strconv.Itoa(p.reconfigured),
		"opened":       strconv.Itoa(p.opened),
		"closed":       strconv.Itoa(p.closed),
		"last_viewer":  p.lastViewer,
	}
}

func New() api.Registration {
	p := &plugin{}
	return api.Registration{
		Init:              p.init,
		OnServerLifecycle: p.lifecycle,
		OnWebsocketOpen:   p.opened_,
		OnWebsocketClose:  p.closed_,
		Diagnostics:       p.diagnostics,
		Routes: []api.Route{
			{Method: "GET", Path: "/health", ServerScoped: true, Handler: p.health},
		},
	}
}
`

// settleAsync waits for fire-and-forget hooks to arrive, polling the plugin's
// own account rather than sleeping a fixed amount.
func settleAsync(t *testing.T, check func(map[string]string) bool) map[string]string {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		fields := Diagnostics()["watcher"]
		if check(fields) || time.Now().After(deadline) {
			return fields
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPluginCanRunACommandInTheContainer(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	// What the container "says" back.
	bridge.execResult = &api.ExecResult{Stdout: "ok\n", ExitCode: 0}

	m, root := newTestManager(t, bridge)
	enableTestPlugin(t, m, root, "watcher", interfacesManifest, interfacesSource)

	_, route, _, ok := m.FindRoute("watcher", "GET", "/health", true)
	if !ok {
		t.Fatal("the health route should resolve")
	}

	res := route.Handler(api.Request{
		Method: "GET", Path: "/health", Server: server, ServerScoped: true,
	})
	if res.Status != 200 {
		t.Fatalf("health returned %d: %s", res.Status, res.Body)
	}
	if got := string(res.Body); got != "ok exit=0" {
		t.Errorf("the plugin did not get the command's output back, got %q", got)
	}

	// And the command actually reached the container rather than the console.
	if len(bridge.execs) != 1 || bridge.execs[0] != "sh -c echo ok" {
		t.Errorf("expected one exec of the command, got %v", bridge.execs)
	}
	if len(bridge.commands) != 0 {
		t.Errorf("an exec must not go through the console, got %v", bridge.commands)
	}
}

func TestPluginHearsAboutAServerBeingReconfigured(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)
	enableTestPlugin(t, m, root, "watcher", interfacesManifest, interfacesSource)

	Lifecycle(api.Lifecycle{Server: server, Event: api.ServerReconfigured})

	fields := settleAsync(t, func(f map[string]string) bool { return f["reconfigured"] == "1" })
	if fields["reconfigured"] != "1" {
		t.Errorf("the plugin should have seen one reconfigure, saw %q", fields["reconfigured"])
	}
}

func TestPluginSeesConsoleConnectionsOpenAndClose(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)
	enableTestPlugin(t, m, root, "watcher", interfacesManifest, interfacesSource)

	if !HasWebsocketLifecycleHooks() {
		t.Fatal("the plugin registered both hooks, so dispatch should report them")
	}

	conn := api.WebsocketConnection{
		Server:      server,
		User:        "viewer-uuid",
		IP:          "198.51.100.7",
		Permissions: []string{"control.console"},
	}

	WebsocketOpened(conn)
	fields := settleAsync(t, func(f map[string]string) bool { return f["opened"] == "1" })
	if fields["opened"] != "1" {
		t.Errorf("the plugin should have seen the connection open, saw %q", fields["opened"])
	}
	if fields["last_viewer"] != "viewer-uuid" {
		t.Errorf("the plugin should know who connected, got %q", fields["last_viewer"])
	}

	WebsocketClosed(conn)
	fields = settleAsync(t, func(f map[string]string) bool { return f["closed"] == "1" })
	if fields["closed"] != "1" {
		t.Errorf("the plugin should have seen the connection close, saw %q", fields["closed"])
	}
}

func TestNoWebsocketLifecycleHooksWhenNoneRegistered(t *testing.T) {
	bridge := newFakeBridge()
	m, root := newTestManager(t, bridge)

	// The fixture registers neither, so the guard the websocket path checks
	// before building a snapshot must stay false.
	enableTestPlugin(t, m, root, "fixture", fixtureManifest, fixtureSource)

	if HasWebsocketLifecycleHooks() {
		t.Error("a plugin registering neither hook should not turn the guard on")
	}
}
