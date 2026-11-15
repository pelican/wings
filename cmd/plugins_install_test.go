package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
	"github.com/pelican/wings/plugin/install"
)

// stagePlugin sets up a plugins directory with the configuration pointing at
// it, an installed "demo" at installed (if not empty), and a staged "demo" at
// version staged, as StageAdd or StageUpdate would leave them.
func stagePlugin(t *testing.T, installed, staged string) (dir string, stage *install.StagedInstall) {
	t.Helper()
	c := usePluginPolicy(t, true, nil, nil)
	dir = t.TempDir()
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
	return dir, &install.StagedInstall{DestDir: filepath.Join(dir, "demo"), StagingDir: filepath.Join(dir, ".wings-install-test")}
}

func installedVersion(t *testing.T, dir string) string {
	t.Helper()
	m, err := plugin.LoadManifest(filepath.Join(dir, "demo", plugin.ManifestFile))
	if err != nil {
		return ""
	}
	return m.Version
}

func TestInstallStagedInstallsAndTrusts(t *testing.T) {
	dir, stage := stagePlugin(t, "1.0.0", "2.0.0")

	m, err := installStaged(stage, nil, true)
	if err != nil {
		t.Fatalf("installStaged: %v", err)
	}
	if got := installedVersion(t, dir); got != "2.0.0" {
		t.Fatalf("installed version = %q, want 2.0.0", got)
	}
	ts, err := plugin.LoadTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if !ts.IsTrusted(m) {
		t.Fatal("the installed plugin is not trusted")
	}
	if _, err := os.Stat(stage.StagingDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory left behind: %v", err)
	}
}

func TestInstallStagedRollsBackWhenTrustCannotBeSaved(t *testing.T) {
	dir, stage := stagePlugin(t, "1.0.0", "2.0.0")
	// A directory where the trust file should be makes saving the approval fail.
	if err := os.Mkdir(filepath.Join(dir, ".trust.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := installStaged(stage, nil, true)
	if err == nil || !strings.Contains(err.Error(), "recording trust") || !strings.Contains(err.Error(), "the previous state was restored") {
		t.Fatalf("installStaged error = %v, want a trust failure with the previous state restored", err)
	}
	if got := installedVersion(t, dir); got != "1.0.0" {
		t.Fatalf("installed version = %q, want the previous 1.0.0 back", got)
	}
	if _, err := os.Stat(stage.StagingDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory left behind: %v", err)
	}
}

func TestInstallStagedStopsWhenTheCheckFails(t *testing.T) {
	dir, stage := stagePlugin(t, "1.0.0", "2.0.0")

	_, err := installStaged(stage, func(plugin.Manifest) error { return errors.New("wrong version") }, true)
	if err == nil || err.Error() != "wrong version" {
		t.Fatalf("installStaged error = %v, want the check's error", err)
	}
	if got := installedVersion(t, dir); got != "1.0.0" {
		t.Fatalf("installed version = %q, want 1.0.0 untouched", got)
	}
	if _, err := os.Stat(stage.StagingDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory left behind: %v", err)
	}
}
