package cmd

import (
	"bytes"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// capturePrompt sends the approval prompt to a buffer for the test, and
// returns the user the test expects plugins to run as: system.user set to
// nobody when the test runs as root, the test's own user otherwise.
func capturePrompt(t *testing.T) (*bytes.Buffer, string) {
	t.Helper()
	var out bytes.Buffer
	prev := promptOut
	promptOut = &out
	t.Cleanup(func() { promptOut = prev })
	if os.Geteuid() == 0 {
		c := config.Get()
		c.System.User.Uid, c.System.User.Gid = 65534, 65534
		config.Set(c)
		return &out, "nobody"
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return &out, u.Username
}

// disclosure is the sentence contract S requires at every approval.
func disclosure(name string) string {
	return "This plugin runs on this node as " + name + ", with that user's access to the machine. " +
		"The host access listed only limits what it can ask Wings."
}

func TestInstallPromptDisclosesABinaryPlugin(t *testing.T) {
	_, stage := stagePlugin(t, "1.0.0", "2.0.0")
	out, runsAs := capturePrompt(t)
	if err := os.WriteFile(filepath.Join(stage.StagingDir, "demo"), []byte("\x7fELF\x02\x01\x01\x00"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installStaged(stage, nil, true); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	t.Log("\n" + text)
	for _, want := range []string{
		"form:        binary",
		"    - demo\n",
		"signature:   not signed",
		"runs as:     " + runsAs + "\n",
		"host access requested: none",
		disclosure(runsAs),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the prompt is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, ".wings-install") {
		t.Errorf("the prompt shows the staging path:\n%s", text)
	}
}

func TestTrustPromptDisclosesASourcePlugin(t *testing.T) {
	dir, _ := stagePlugin(t, "1.0.0", "2.0.0")
	out, runsAs := capturePrompt(t)
	m, err := plugin.LoadManifest(filepath.Join(dir, "demo", plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	m.Requires.HostAPI = []string{"server.console.read"}
	if _, err := approvePlugin(m, true); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"form:        source",
		"signature:   not checked",
		"runs as:     " + runsAs + "\n",
		"    - server.console.read\n",
		disclosure(runsAs),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the prompt is missing %q:\n%s", want, text)
		}
	}
}
