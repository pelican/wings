package external

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelican/wings/plugin"
)

func TestEventListenerScopeComesFromManifest(t *testing.T) {
	c := NewClient(plugin.Manifest{
		Name:       "audit",
		Capability: plugin.CapabilityEvents,
		Provides: plugin.Provides{
			Events: []string{"server.state"},
		},
	}, nil)

	s := c.EventListener()
	if s.Name() != "audit" {
		t.Fatalf("name = %q, want manifest name", s.Name())
	}
	got := s.Events()
	if len(got) != 1 || got[0] != "server.state" {
		t.Fatalf("events = %v, want manifest scope only", got)
	}
}

func TestEventListenerDefaultsToAllEvents(t *testing.T) {
	c := NewClient(plugin.Manifest{Name: "audit", Capability: plugin.CapabilityEvents}, nil)
	got := c.EventListener().Events()
	if len(got) != 1 || got[0] != "*" {
		t.Fatalf("events = %v, want [*] when the manifest names none", got)
	}
}

func TestPipeLinesKeepsReadingAfterAnOversizedLine(t *testing.T) {
	long := strings.Repeat("x", 100<<10)
	input := "first\n" + long + "\nafter\n"
	var got []string
	pipeLines(strings.NewReader(input), 1024, func(l string) { got = append(got, l) })

	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(got), got)
	}
	if got[0] != "first" || got[2] != "after" {
		t.Fatalf("lines around the long one were lost: %q / %q", got[0], got[2])
	}
	if !strings.HasSuffix(got[1], "(line truncated)") || len(got[1]) > 1024+64 {
		t.Fatalf("the long line should be cut and marked, got %d bytes", len(got[1]))
	}
}

// TestPluginGetsADataDirectory covers where a plugin keeps settings and state:
// outside its own directory, which updates replace and the approval hashes.
func TestPluginGetsADataDirectory(t *testing.T) {
	plugins := t.TempDir()
	dir := filepath.Join(plugins, "foo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: v1\nname: foo\ncapability: events\n" +
		"entrypoint: [\"sh\", \"-c\", \"echo ok > \\\"$WINGS_PLUGIN_DATA/seen\\\"\"]\n"
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := plugin.LoadManifest(filepath.Join(dir, plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(plugins, plugin.DataDirName, "foo")
	if m.DataDir() != want {
		t.Fatalf("DataDir() = %q, want %q", m.DataDir(), want)
	}

	// The plugin exits without answering initialize; it has run by then.
	_ = NewClient(m, nil).EventListener().OnEvent(context.Background(), plugin.Event{Type: "server.state"})
	b, err := os.ReadFile(filepath.Join(want, "seen"))
	if err != nil || strings.TrimSpace(string(b)) != "ok" {
		t.Fatalf("the plugin could not write to WINGS_PLUGIN_DATA: %q %v", b, err)
	}
	info, err := os.Stat(want)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("data directory mode = %v, want 0700 (%v)", info.Mode().Perm(), err)
	}
}

// TestCloseWaitsForStderrBeforeWait covers os/exec's rule that Wait must not
// run before every read from a pipe has finished: Wait closes stderr, so
// whatever the plugin printed last could be lost.
func TestCloseWaitsForStderrBeforeWait(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if _, err := cmd.StderrPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stderrDone := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond) // the reader still has lines to log
		close(stderrDone)
	}()
	start := time.Now()
	_ = processCloser{cmd: cmd, stderrDone: stderrDone}.Close()
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("Close called Wait after %v, before the stderr reader finished", d)
	}
}
