package cmd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakePanel answers the node configuration request, with a plugins block that
// wings configure must ignore, and counts the requests.
func fakePanel(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uuid":"node-uuid","token_id":"tid","token":"tok","plugins":{"panel_access":"full","disabled":["from-panel"]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// runConfigure runs `wings configure` against panel, writing to path.
func runConfigure(t *testing.T, panel, path, access string) {
	t.Helper()
	savedArgs, savedPath := configureArgs, configPath
	t.Cleanup(func() { configureArgs, configPath = savedArgs, savedPath })
	configureArgs.PanelURL = panel
	configureArgs.Token = "peli_" + strings.Repeat("a", 43)
	configureArgs.Node = "1"
	configureArgs.ConfigPath = path
	configureArgs.Override = true
	configureArgs.PluginAccess = access
	configPath = path
	configureCmdRun(nil, nil)
}

func TestConfigurePluginAccessFlag(t *testing.T) {
	srv, _ := fakePanel(t)
	path := filepath.Join(t.TempDir(), "config.yml")

	// A new node: the flag sets the level, the Panel's plugins block is ignored.
	runConfigure(t, srv.URL, path, "signed")
	p := onDisk(t, path)
	if p.PanelAccess != "signed" || len(p.Disabled) != 0 {
		t.Fatalf("plugins block = %+v, want panel_access signed and nothing from the Panel", p)
	}

	// Configuring again without the flag keeps the operator's value.
	runConfigure(t, srv.URL, path, "")
	if got := onDisk(t, path).PanelAccess; got != "signed" {
		t.Fatalf("panel_access = %q after configure without the flag, want signed kept", got)
	}

	runConfigure(t, srv.URL, path, "OFF")
	if got := onDisk(t, path).PanelAccess; got != "off" {
		t.Fatalf("panel_access = %q, want off", got)
	}
}

func TestConfigureWithoutTheFlagUsesTheDefault(t *testing.T) {
	srv, _ := fakePanel(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	runConfigure(t, srv.URL, path, "")
	if got := onDisk(t, path).PanelAccess; got != "view" {
		t.Fatalf("panel_access = %q on a new node, want the default view", got)
	}
}

// TestConfigureChecksPluginAccessFirst runs configure with a bad level in a
// child process (it exits): it must fail without asking the Panel anything.
func TestConfigureChecksPluginAccessFirst(t *testing.T) {
	if os.Getenv("WINGS_TEST_CONFIGURE_CHILD") == "1" {
		runConfigure(t, os.Getenv("WINGS_TEST_PANEL"), os.Getenv("WINGS_TEST_CONFIG"), "everything")
		os.Exit(0)
	}
	srv, hits := fakePanel(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	cmd := exec.Command(os.Args[0], "-test.run=^TestConfigureChecksPluginAccessFirst$")
	cmd.Env = append(os.Environ(), "WINGS_TEST_CONFIGURE_CHILD=1", "WINGS_TEST_PANEL="+srv.URL, "WINGS_TEST_CONFIG="+path)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("configure with a bad level: %v, want exit status 1\n%s", err, out)
	}
	if !strings.Contains(string(out), "Invalid --plugin-access") {
		t.Fatalf("output does not name the bad flag:\n%s", out)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the Panel was asked %d times before the flag was checked", n)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a config file was written: %v", err)
	}
}
