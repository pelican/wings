package install

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// stageDemo makes a plugins directory with "demo" installed at installed (if
// set) and a staged "demo" at staged, as StageAdd or StageUpdate leave them.
func stageDemo(t *testing.T, installed, staged string) (string, *StagedInstall) {
	t.Helper()
	c := setConfig(t)
	dir := t.TempDir()
	c.Plugins.Directory = dir
	config.Set(c)
	write := func(sub, version string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := "apiVersion: v1\nname: demo\ncapability: events\nversion: " + version + "\nentrypoint: [\"./demo\"]\n"
		if err := os.WriteFile(filepath.Join(dir, sub, plugin.ManifestFile), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if installed != "" {
		write("demo", installed)
	}
	write(".wings-install-test", staged)
	return dir, &StagedInstall{DestDir: filepath.Join(dir, "demo"), StagingDir: filepath.Join(dir, ".wings-install-test")}
}

func TestInstallNotApprovedLeavesEverythingAsItWas(t *testing.T) {
	dir, stage := stageDemo(t, "", "1.0.0")
	var shown plugin.Manifest
	_, err := Install(stage, nil, func(m plugin.Manifest) (bool, error) { shown = m; return false, nil })
	if !errors.Is(err, ErrNotApproved) {
		t.Fatalf("Install error = %v, want ErrNotApproved", err)
	}
	if shown.Name != "demo" || shown.Version != "1.0.0" {
		t.Fatalf("the approver was shown %+v, want the staged manifest", shown)
	}
	if _, err := os.Stat(filepath.Join(dir, "demo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a declined plugin was installed: %v", err)
	}
	if _, err := os.Stat(stage.StagingDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory left behind: %v", err)
	}
	ts, _ := plugin.LoadTrustStore()
	if len(ts.Approved) != 0 {
		t.Fatalf("a declined plugin was trusted: %v", ts.Approved)
	}
}

func TestInstallApprovedInstallsAndTrusts(t *testing.T) {
	dir, stage := stageDemo(t, "1.0.0", "2.0.0")
	m, err := Install(stage, nil, func(plugin.Manifest) (bool, error) { return true, nil })
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	installed, err := plugin.LoadManifest(filepath.Join(dir, "demo", plugin.ManifestFile))
	if err != nil || installed.Version != "2.0.0" {
		t.Fatalf("installed = %+v, %v; want 2.0.0", installed, err)
	}
	ts, _ := plugin.LoadTrustStore()
	if !ts.IsTrusted(m) {
		t.Fatal("the installed plugin is not trusted")
	}
}

func TestToggle(t *testing.T) {
	p := config.PluginConfiguration{Autoload: true, Disabled: []string{"a"}}
	Toggle(&p, "a", true)
	if len(p.Disabled) != 0 || len(p.Enabled) != 0 {
		t.Fatalf("enabling with autoload on should only unblock: %+v", p)
	}
	Toggle(&p, "a", false)
	Toggle(&p, "a", false)
	if len(p.Disabled) != 1 {
		t.Fatalf("disabling twice should list it once: %+v", p)
	}
	p = config.PluginConfiguration{Autoload: false}
	Toggle(&p, "b", true)
	if len(p.Enabled) != 1 || p.Enabled[0] != "b" {
		t.Fatalf("enabling with autoload off should allowlist it: %+v", p)
	}
}
