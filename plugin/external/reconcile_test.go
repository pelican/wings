package external

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// shPlugin is an external plugin in sh: it answers initialize, writes its pid
// to its data directory and appends every message it gets after that to
// <data>/events.
const shPlugin = `IFS= read -r line || exit 0
id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
printf '{"jsonrpc":"2.0","id":%s,"result":{"name":"x","capability":"events"}}\n' "$id"
echo $$ > "$WINGS_PLUGIN_DATA/pid"
while IFS= read -r line; do printf '%s\n' "$line" >> "$WINGS_PLUGIN_DATA/events"; done
`

// usePluginsDir installs a configuration whose plugins directory is a new
// temporary directory, writes it to disk, and returns the directory and the
// configuration file's path.
func usePluginsDir(t *testing.T, change func(*config.PluginConfiguration)) (string, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.yml")
	c, err := config.NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = "test-token"
	c.Plugins.Directory = filepath.Join(root, "plugins")
	// As root (in a container), plugins would run as system.user, which
	// can't enter t.TempDir's directories; see TestMain.
	c.Plugins.RunAsRoot = os.Geteuid() == 0
	if change != nil {
		change(&c.Plugins)
	}
	if err := os.MkdirAll(c.Plugins.Directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteToDisk(c); err != nil {
		t.Fatal(err)
	}
	config.Set(c)
	// Stop whatever this test registered, so no plugin process outlives it.
	t.Cleanup(func() {
		config.Update(func(c *config.Configuration) { c.Plugins.Autoload, c.Plugins.Enabled = false, nil })
		_ = Reconcile(filepath.Join(root, "none"), nil)
	})
	return c.Plugins.Directory, path
}

// writePlugin installs the sh plugin under name with the given version.
func writePlugin(t *testing.T, dir, name, version string) plugin.Manifest {
	t.Helper()
	pd := filepath.Join(dir, name)
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pd, "run.sh"), []byte(shPlugin), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("apiVersion: v1\nname: %s\ncapability: events\nversion: %s\nentrypoint: [\"sh\", \"run.sh\"]\nrequires: { hostAPI: [\"server.stats\", \"server.logs\"] }\n", name, version)
	if err := os.WriteFile(filepath.Join(pd, plugin.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := plugin.LoadManifest(filepath.Join(pd, plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func approve(t *testing.T, m plugin.Manifest) {
	t.Helper()
	ts, err := plugin.LoadTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Approve(m); err != nil {
		t.Fatal(err)
	}
}

func listener(name string) plugin.EventListener {
	l, _ := plugin.Default().EventListener(name)
	return l
}

func readPid(t *testing.T, m plugin.Manifest) int {
	t.Helper()
	var pid int
	eventually(t, "the plugin's pid file", func() bool {
		b, err := os.ReadFile(filepath.Join(m.DataDir(), "pid"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	})
	return pid
}

func alive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func eventCount(m plugin.Manifest) int {
	b, _ := os.ReadFile(filepath.Join(m.DataDir(), "events"))
	return strings.Count(string(b), "\n")
}

// TestReconcileEnablesAndDisablesWithoutRestart covers a plugin enabled and
// then disabled while Wings runs: it starts getting events once enabled, an
// unchanged running plugin is not restarted, and disabling stops its process.
func TestReconcileEnablesAndDisablesWithoutRestart(t *testing.T) {
	dir, _ := usePluginsDir(t, func(p *config.PluginConfiguration) { p.Disabled = []string{"rec-live"} })
	m := writePlugin(t, dir, "rec-live", "1.0.0")
	approve(t, m)

	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	if listener("rec-live") != nil {
		t.Fatal("a disabled plugin was registered")
	}

	config.Update(func(c *config.Configuration) { c.Plugins.Disabled = nil })
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	first := listener("rec-live")
	if first == nil {
		t.Fatal("the plugin was not registered after it was enabled")
	}
	plugin.Emit(plugin.Event{Type: "server.state", Server: "a"})
	pid := readPid(t, m)
	eventually(t, "the first event", func() bool { return eventCount(m) == 1 })

	// Nothing about it changed: same listener, same process.
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	if listener("rec-live") != first {
		t.Fatal("an unchanged plugin was registered again")
	}
	plugin.Emit(plugin.Event{Type: "server.state", Server: "b"})
	eventually(t, "the second event", func() bool { return eventCount(m) == 2 })
	if got := readPid(t, m); got != pid || !alive(pid) {
		t.Fatalf("an unchanged plugin was restarted (pid %d, then %d)", pid, got)
	}

	config.Update(func(c *config.Configuration) { c.Plugins.Disabled = []string{"rec-live"} })
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	if listener("rec-live") != nil {
		t.Fatal("a disabled plugin is still registered")
	}
	if alive(pid) {
		t.Fatal("a disabled plugin's process is still running")
	}
	plugin.Emit(plugin.Event{Type: "server.state", Server: "c"})
	time.Sleep(100 * time.Millisecond)
	if n := eventCount(m); n != 2 {
		t.Fatalf("the plugin got %d events, want 2: none after it was disabled", n)
	}
}

// TestReconcileFollowsTheApproval covers a plugin whose files change: it stops
// until the operator approves the new files, then the new version loads.
func TestReconcileFollowsTheApproval(t *testing.T) {
	dir, _ := usePluginsDir(t, nil)
	m := writePlugin(t, dir, "rec-trust", "1.0.0")

	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	if listener("rec-trust") != nil {
		t.Fatal("an untrusted plugin was registered")
	}
	approve(t, m)
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	v1 := listener("rec-trust")
	if v1 == nil {
		t.Fatal("the plugin was not registered once trusted")
	}

	m = writePlugin(t, dir, "rec-trust", "1.1.0")
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	if listener("rec-trust") != nil {
		t.Fatal("the plugin kept running after its files changed without a new approval")
	}

	approve(t, m)
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	v2 := listener("rec-trust")
	if v2 == nil || v2 == v1 {
		t.Fatal("the re-approved version was not registered as a new plugin")
	}

	ts, err := plugin.LoadTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Revoke("rec-trust"); err != nil {
		t.Fatal(err)
	}
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	if listener("rec-trust") != nil {
		t.Fatal("an untrusted plugin is still registered")
	}
}

// TestReconcileReloadsWhenGrantsChange covers plugins.grants: the host methods
// a plugin may call are fixed when its process is set up, so narrowing them
// has to replace the running one.
func TestReconcileReloadsWhenGrantsChange(t *testing.T) {
	dir, _ := usePluginsDir(t, nil)
	m := writePlugin(t, dir, "rec-grants", "1.0.0")
	approve(t, m)
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	before := listener("rec-grants")

	config.Update(func(c *config.Configuration) {
		c.Plugins.Grants = map[string][]string{"rec-grants": {"server.stats"}}
	})
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	if after := listener("rec-grants"); after == nil || after == before {
		t.Fatal("the plugin was not replaced after its grants changed")
	}
}

// TestReconcileKeepsPluginsWhenTheDirectoryCannotBeListed covers a failed
// directory read: a plugin that is still enabled and approved keeps running,
// one that was disabled is still stopped.
func TestReconcileKeepsPluginsWhenTheDirectoryCannotBeListed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can list a directory without read permission")
	}
	dir, _ := usePluginsDir(t, nil)
	for _, name := range []string{"rec-keep", "rec-drop"} {
		approve(t, writePlugin(t, dir, name, "1.0.0"))
	}
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	// Write and search, no read: the trust store opens by name, the
	// directory can't be listed.
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	config.Update(func(c *config.Configuration) { c.Plugins.Disabled = []string{"rec-drop"} })

	if err := Reconcile(dir, nil); err == nil {
		t.Fatal("Reconcile did not report the directory read error")
	}
	if listener("rec-keep") == nil {
		t.Fatal("an enabled, approved plugin was stopped because the directory could not be listed")
	}
	if listener("rec-drop") != nil {
		t.Fatal("a disabled plugin kept running because the directory could not be listed")
	}
}

func TestClosedClientDoesNotStartThePlugin(t *testing.T) {
	dir, _ := usePluginsDir(t, nil)
	m := writePlugin(t, dir, "rec-closed", "1.0.0")
	c := NewClient(m, nil)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.EventListener().OnEvent(context.Background(), plugin.Event{Type: "server.state"}); err != nil {
		t.Fatalf("an event for a stopped plugin returned %v, want it dropped quietly", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(m.DataDir(), "pid")); err == nil {
		t.Fatal("a closed client started the plugin")
	}
}

// TestWatchPolicyAppliesChangesFromOtherProcesses covers the daemon noticing
// what `wings plugins disable` and `wings plugins trust` change on disk, and
// ignoring its own writes that leave the plugins block alone.
func TestWatchPolicyAppliesChangesFromOtherProcesses(t *testing.T) {
	dir, path := usePluginsDir(t, nil)
	applied := make(chan struct{}, 16)
	plugin.AfterPolicyChange = func() { applied <- struct{}{} }
	t.Cleanup(func() { plugin.AfterPolicyChange = nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = WatchPolicy(ctx, path, 20*time.Millisecond)
	}()
	t.Cleanup(func() { cancel(); <-done })

	wait := func(what string) {
		t.Helper()
		select {
		case <-applied:
		case <-time.After(5 * time.Second):
			t.Fatalf("the watcher did not apply %s", what)
		}
	}
	wait("the reconcile at start")
	time.Sleep(60 * time.Millisecond) // let it take its first look

	// Another process (here: the same code the CLI runs) disables a plugin.
	other, err := config.NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.UpdatePluginsOnDisk(other, func(p *config.PluginConfiguration) {
		p.Disabled = []string{"watched"}
	}); err != nil {
		t.Fatal(err)
	}
	wait("the config file change")
	if plugin.Enabled("watched") {
		t.Fatal("the running daemon still has the plugin enabled")
	}

	// A write that keeps the plugins block (the daemon at boot, a Panel push).
	if err := config.WriteToDiskKeepingPlugins(config.Get()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	select {
	case <-applied:
		t.Fatal("a write that left the plugins block alone was applied")
	default:
	}

	// `wings plugins trust` writes only the trust store.
	approve(t, writePlugin(t, dir, "watched-trust", "1.0.0"))
	wait("the trust store change")
}
