package external

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// clientDir is the directory the registered client for name runs its plugin
// in, or "" if none is registered.
func clientDir(name string) string {
	l, ok := listener(name).(eventListener)
	if !ok {
		return ""
	}
	return l.c.manifest.Dir()
}

// watch runs WatchPolicy on path every interval, with plugin.AfterPolicyChange
// reconciling the plugins directory as the daemon does, until the test ends.
func watch(t *testing.T, path string, interval time.Duration) {
	t.Helper()
	plugin.AfterPolicyChange = func() { _ = Reconcile(config.Get().Plugins.Directory, nil) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = WatchPolicy(ctx, path, interval)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		plugin.AfterPolicyChange = nil
	})
}

// A plugin whose grants were narrowed while the plugins directory can't be
// listed can't be shown to still have only what it was given: it is stopped.
func TestReconcileStopsPluginsWhoseGrantsChangedWhileTheDirectoryCannotBeListed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can list a directory without read permission")
	}
	dir, _ := usePluginsDir(t, nil)
	approve(t, writePlugin(t, dir, "rec-grant-dark", "1.0.0"))
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	if listener("rec-grant-dark") == nil {
		t.Fatal("the plugin was not registered")
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	config.Update(func(c *config.Configuration) {
		c.Plugins.Grants = map[string][]string{"rec-grant-dark": {"server.stats"}}
	})
	if err := Reconcile(dir, nil); err == nil {
		t.Fatal("Reconcile did not report the directory read error")
	}
	if listener("rec-grant-dark") != nil {
		t.Fatal("a plugin kept its old host grants after plugins.grants narrowed them")
	}
}

// A plugin whose files changed while the directory can't be listed is stopped
// too: its approval is checked against what is on disk, not just the store.
func TestReconcileStopsChangedPluginsWhileTheDirectoryCannotBeListed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can list a directory without read permission")
	}
	dir, _ := usePluginsDir(t, nil)
	m := writePlugin(t, dir, "rec-edit-dark", "1.0.0")
	approve(t, m)
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Dir(), "run.sh"), []byte("echo changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_ = Reconcile(dir, nil)
	if listener("rec-edit-dark") != nil {
		t.Fatal("a plugin whose files changed kept running because the directory could not be listed")
	}
}

// When the plugins directory moves, a running plugin must be replaced so its
// process runs in, and keeps its data under, the new directory.
func TestReconcileReplacesAPluginWhoseDirectoryMoved(t *testing.T) {
	dir, _ := usePluginsDir(t, nil)
	approve(t, writePlugin(t, dir, "rec-moved", "1.0.0"))
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	config.Update(func(c *config.Configuration) { c.Plugins.Directory = moved })
	if err := Reconcile(moved, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := clientDir("rec-moved"), filepath.Join(moved, "rec-moved"); got != want {
		t.Fatalf("the plugin runs in %q, want %q", got, want)
	}
}

// A change made between the daemon's first reconcile and the watcher's start
// (here: the approval revoked) must still be applied.
func TestWatchPolicyReconcilesOnceAtStart(t *testing.T) {
	dir, path := usePluginsDir(t, nil)
	approve(t, writePlugin(t, dir, "watch-start", "1.0.0"))
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	ts, err := plugin.LoadTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Revoke("watch-start"); err != nil {
		t.Fatal(err)
	}
	watch(t, path, time.Hour) // no poll runs; only the start
	eventually(t, "the revoked plugin to stop", func() bool { return listener("watch-start") == nil })
}

// Editing an installed plugin's files without a new approval stops it, for
// the manifest, its entrypoint, and a file deeper in its directory.
func TestWatchPolicyStopsAPluginWhoseFilesChanged(t *testing.T) {
	prev := deepScanEvery
	deepScanEvery = 3
	t.Cleanup(func() { deepScanEvery = prev })
	appendTo := func(rel string) func(*testing.T, string) {
		return func(t *testing.T, pd string) {
			f, err := os.OpenFile(filepath.Join(pd, rel), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("# changed\n")
			_ = f.Close()
		}
	}
	edits := map[string]func(*testing.T, string){
		"manifest":    appendTo(plugin.ManifestFile),
		"entrypoint":  appendTo("run.sh"),
		"nested file": appendTo(filepath.Join("lib", "util.sh")),
	}
	for what, edit := range edits {
		t.Run(what, func(t *testing.T) {
			dir, path := usePluginsDir(t, nil)
			m := writePlugin(t, dir, "watch-edit", "1.0.0")
			if err := os.MkdirAll(filepath.Join(m.Dir(), "lib"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(m.Dir(), "lib", "util.sh"), []byte("true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			approve(t, m)
			watch(t, path, 20*time.Millisecond)
			eventually(t, "the plugin to load", func() bool { return listener("watch-edit") != nil })
			time.Sleep(100 * time.Millisecond) // let it take its first look
			edit(t, m.Dir())
			eventually(t, "the changed plugin to stop", func() bool { return listener("watch-edit") == nil })
		})
	}
}

// A registered plugin whose files changed is not started, even before the
// watcher notices the change.
func TestClientDoesNotStartAChangedPlugin(t *testing.T) {
	dir, _ := usePluginsDir(t, nil)
	m := writePlugin(t, dir, "rec-spawn", "1.0.0")
	approve(t, m)
	if err := Reconcile(dir, nil); err != nil {
		t.Fatal(err)
	}
	l := listener("rec-spawn")
	if l == nil {
		t.Fatal("the plugin was not registered")
	}
	if err := os.WriteFile(filepath.Join(m.Dir(), "run.sh"), []byte(shPlugin+"# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.OnEvent(context.Background(), plugin.Event{Type: "server.state"}); err == nil {
		t.Fatal("an event for a changed plugin was delivered")
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(m.DataDir(), "pid")); err == nil {
		t.Fatal("the changed plugin was started")
	}
}

// TestWatchPolicySeesAnEditWithTheSameSizeAndTime: Linux keeps file times to
// a clock tick, and the config file is rewritten in place, so two writes
// within one tick that leave it the same size look like no change by time
// and size. The watcher must compare the block itself.
func TestWatchPolicySeesAnEditWithTheSameSizeAndTime(t *testing.T) {
	dir, path := usePluginsDir(t, func(p *config.PluginConfiguration) { p.Disabled = []string{"watch-same"} })
	approve(t, writePlugin(t, dir, "watch-same", "1.0.0"))
	plugin.AfterPolicyChange = func() { _ = Reconcile(config.Get().Plugins.Directory, nil) }
	t.Cleanup(func() { plugin.AfterPolicyChange = nil })
	c, err := config.NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	w := policyWatch{path: path, c: c}
	w.start()
	if listener("watch-same") != nil {
		t.Fatal("a disabled plugin was loaded")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same length: the plugin is no longer disabled.
	edited := strings.Replace(string(b), "- watch-same", "- other-same", 1)
	if edited == string(b) {
		t.Fatal("the disabled entry wasn't found in the config file")
	}
	if err := os.WriteFile(path, []byte(edited), info.Mode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	w.poll()
	if listener("watch-same") == nil {
		t.Fatal("an edit with the same size and time was missed")
	}
}

// TestWatchPolicyRestoresTheFileWhenTheRunningBlockDiffers: the running
// plugins block was changed in this process without the file (here as a
// test standing in for another process does), and a plugin was loaded from
// it. The file still says disabled, the same as the watcher's last read; the
// next poll must still bring the daemon back to the file.
func TestWatchPolicyRestoresTheFileWhenTheRunningBlockDiffers(t *testing.T) {
	_, path := usePluginsDir(t, func(p *config.PluginConfiguration) { p.Disabled = []string{"watch-mem"} })
	approve(t, writePlugin(t, config.Get().Plugins.Directory, "watch-mem", "1.0.0"))
	plugin.AfterPolicyChange = func() { _ = Reconcile(config.Get().Plugins.Directory, nil) }
	t.Cleanup(func() { plugin.AfterPolicyChange = nil })
	c, err := config.NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	w := policyWatch{path: path, c: c}
	w.start()
	config.Update(func(c *config.Configuration) { c.Plugins.Disabled = nil })
	plugin.PolicyChanged()
	if listener("watch-mem") == nil {
		t.Fatal("the plugin enabled in memory was not loaded")
	}
	w.poll()
	if listener("watch-mem") != nil {
		t.Fatal("the plugin the file disables is still loaded")
	}
}
