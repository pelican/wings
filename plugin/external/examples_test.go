package external

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelican/wings/plugin"
)

// recordingHost answers every host callback with a fixed console line and
// records what was asked for.
type recordingHost struct {
	mu    sync.Mutex
	calls []string
}

func (h *recordingHost) Invoke(_ context.Context, method string, params json.RawMessage) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, method+" "+string(params))
	return map[string]any{"lines": []string{"server stopped"}}, nil
}

func (h *recordingHost) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// install copies examples/plugins/<example> into a temporary plugins directory
// and loads its manifest, the way Wings finds an installed plugin.
func install(t *testing.T, example string) plugin.Manifest {
	t.Helper()
	src := filepath.Join("..", "..", "examples", "plugins", example)
	dst := filepath.Join(t.TempDir(), "plugins", example)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "__pycache__") {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy %s: %v", example, err)
	}
	m, err := plugin.LoadManifest(filepath.Join(dst, "plugin.yaml"))
	if err != nil {
		t.Fatalf("load %s manifest: %v", example, err)
	}
	return m
}

func need(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s is not installed", tool)
	}
}

// run starts the plugin under the real client and delivers one event.
func run(t *testing.T, m plugin.Manifest, host plugin.HostAPI, e plugin.Event) {
	t.Helper()
	c := NewClient(m, plugin.HostHandler(host, m.Requires.HostAPI))
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.EventListener().OnEvent(ctx, e); err != nil {
		t.Fatalf("deliver event: %v", err)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestExamplePluginsRunUnderTheClient starts each external example the way
// Wings does and checks it reacts to an event. A test is skipped when its
// runtime is not installed.
func TestExamplePluginsRunUnderTheClient(t *testing.T) {
	t.Run("python install-log", func(t *testing.T) {
		need(t, "python3")
		m := install(t, "python-install-log")
		run(t, m, nil, plugin.Event{Type: "server.install", Server: "abc", Data: map[string]any{"status": "completed"}})

		logFile := filepath.Join(m.DataDir(), "installs.log")
		eventually(t, "installs.log", func() bool {
			b, err := os.ReadFile(logFile)
			return err == nil && strings.Contains(string(b), "abc install completed")
		})
	})

	t.Run("go external statecount", func(t *testing.T) {
		need(t, "go")
		m := install(t, "go-external-statecount")
		build := exec.Command("go", "build", "-o", filepath.Join(m.Dir(), "wings-plugin-statecount"),
			"../../examples/plugins/go-external-statecount")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
		run(t, m, nil, plugin.Event{Type: "server.state", Server: "abc", Data: map[string]any{"state": "running"}})

		counts := filepath.Join(m.DataDir(), "counts.json")
		eventually(t, "counts.json", func() bool {
			b, err := os.ReadFile(counts)
			if err != nil {
				return false
			}
			var got map[string]map[string]int
			return json.Unmarshal(b, &got) == nil && got["abc"]["running"] == 1
		})
	})

	t.Run("bun state-log", func(t *testing.T) {
		need(t, "bun")
		m := install(t, "state-log")
		host := &recordingHost{}
		run(t, m, host, plugin.Event{Type: "server.state", Server: "abc", Data: map[string]any{"state": "offline"}})

		eventually(t, "a server.logs host call", func() bool {
			for _, c := range host.snapshot() {
				if strings.HasPrefix(c, "server.logs ") && strings.Contains(c, `"server":"abc"`) {
					return true
				}
			}
			return false
		})
	})
}
