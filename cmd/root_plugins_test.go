package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
	"github.com/pelican/wings/plugin/external"
)

// TestCLIPluginChangesApplyWithoutRestart runs the daemon's reload against the
// changes `wings plugins enable/disable/untrust` make to the files, the way a
// second process makes them: the running daemon loads and stops the external
// plugin without a restart.
func TestCLIPluginChangesApplyWithoutRestart(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yml")
	c, err := config.NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = "test-token"
	c.Plugins.Directory = filepath.Join(root, "plugins")
	c.Plugins.Disabled = []string{"cli-live"}
	if err := config.WriteToDisk(c); err != nil {
		t.Fatal(err)
	}
	config.Set(c)

	pd := filepath.Join(c.Plugins.Directory, "cli-live")
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: v1\nname: cli-live\ncapability: events\nversion: 1.0.0\nentrypoint: [\"sh\", \"-c\", \"exit 0\"]\n"
	if err := os.WriteFile(filepath.Join(pd, plugin.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := plugin.LoadManifest(filepath.Join(pd, plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := recordPluginTrust(m); err != nil {
		t.Fatal(err)
	}

	// The daemon at startup.
	if err := external.LoadEnabled(c.Plugins.Directory, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	startPluginPolicyReload(ctx, path, nil, 20*time.Millisecond)
	t.Cleanup(func() {
		cancel()
		plugin.AfterPolicyChange = nil
		config.Update(func(c *config.Configuration) { c.Plugins.Autoload, c.Plugins.Enabled = false, nil })
		_ = external.Reconcile(filepath.Join(root, "none"), nil)
	})

	registered := func() bool {
		_, ok := plugin.Default().EventListener("cli-live")
		return ok
	}
	waitFor := func(what string, want bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for registered() != want {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for the daemon to %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if registered() {
		t.Fatal("a disabled plugin was loaded at startup")
	}

	// Another process: what `wings plugins enable` writes.
	cli, err := config.NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	toggle := func(enable bool) {
		t.Helper()
		if _, err := config.UpdatePluginsOnDisk(cli, func(p *config.PluginConfiguration) {
			togglePlugin(p, "cli-live", enable)
		}); err != nil {
			t.Fatal(err)
		}
	}
	toggle(true)
	waitFor("load the enabled plugin", true)

	toggle(false)
	waitFor("stop the disabled plugin", false)

	toggle(true)
	waitFor("load the plugin enabled again", true)

	// `wings plugins untrust`.
	ts, err := plugin.LoadTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Revoke("cli-live"); err != nil {
		t.Fatal(err)
	}
	waitFor("stop the untrusted plugin", false)

	// `wings plugins trust`.
	if err := recordPluginTrust(m); err != nil {
		t.Fatal(err)
	}
	waitFor("load the trusted plugin", true)
}
