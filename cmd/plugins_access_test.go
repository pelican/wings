package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// useConfigFile writes a default configuration to a temp file, installs it
// and returns its path.
func useConfigFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	c, err := config.NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = "test-token"
	if err := config.WriteToDisk(c); err != nil {
		t.Fatal(err)
	}
	config.Set(c)
	return path
}

func onDisk(t *testing.T, path string) config.PluginConfiguration {
	t.Helper()
	c, err := config.NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := config.PluginsOnDisk(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPanelAccessSetsTheLevelInTheFile(t *testing.T) {
	path := useConfigFile(t)
	var out, errOut bytes.Buffer
	if err := runPanelAccess(&out, &errOut, path, []string{"Signed"}); err != nil {
		t.Fatal(err)
	}
	if got := onDisk(t, path).PanelAccess; got != "signed" {
		t.Fatalf("plugins.panel_access in the file = %q, want signed", got)
	}
	if got := plugin.CurrentPanelAccess(); got != plugin.PanelAccessSigned {
		t.Fatalf("CurrentPanelAccess() = %q after setting it, want signed", got)
	}
	if !strings.Contains(out.String(), "set plugins.panel_access to signed") || errOut.Len() != 0 {
		t.Fatalf("output = %q, errors = %q", out.String(), errOut.String())
	}

	out.Reset()
	if err := runPanelAccess(&out, &errOut, path, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "signed (plugins.panel_access in "+path+")") {
		t.Fatalf("show = %q, want the level and the file it comes from", out.String())
	}
}

func TestPanelAccessRejectsAnUnknownLevel(t *testing.T) {
	path := useConfigFile(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := runPanelAccess(&out, &errOut, path, []string{"everything"}); err == nil {
		t.Fatal("an unknown level was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the config file changed after an unknown level was rejected")
	}
}

func TestPanelAccessSaysWhenTheEnvironmentOverridesIt(t *testing.T) {
	path := useConfigFile(t)
	t.Setenv(plugin.PanelAccessEnv, "off")

	var out, errOut bytes.Buffer
	if err := runPanelAccess(&out, &errOut, path, []string{"full"}); err != nil {
		t.Fatal(err)
	}
	if got := onDisk(t, path).PanelAccess; got != "full" {
		t.Fatalf("plugins.panel_access in the file = %q, want full", got)
	}
	if !strings.Contains(errOut.String(), plugin.PanelAccessEnv+"=off") {
		t.Fatalf("no warning that the environment overrides the file: %q", errOut.String())
	}

	out.Reset()
	if err := runPanelAccess(&out, &errOut, path, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "off ("+plugin.PanelAccessEnv) {
		t.Fatalf("show = %q, want off from the environment", out.String())
	}
}

func TestPanelAccessShowsAnInvalidValueAsOff(t *testing.T) {
	path := useConfigFile(t)
	config.Update(func(c *config.Configuration) { c.Plugins.PanelAccess = "al" })
	var out, errOut bytes.Buffer
	if err := runPanelAccess(&out, &errOut, path, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "off (") || !strings.Contains(out.String(), `invalid value "al"`) {
		t.Fatalf("show = %q, want off and the invalid value", out.String())
	}
}
