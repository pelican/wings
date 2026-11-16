package cmd

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pelican/wings/events"
	"github.com/pelican/wings/server/filesystem"
)

// denylistFS returns a server filesystem whose denylist hides secret.txt, with
// that file and a sub directory holding another copy of it already in place.
func denylistFS(t *testing.T) (*filesystem.Filesystem, string) {
	t.Helper()
	usePluginPolicy(t, true, nil, nil) // filesystem.New reads the global config
	root := t.TempDir()
	for _, p := range []string{"secret.txt", "data/secret.txt", "data/keep.txt", "notes.txt"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte("contents of "+p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := filesystem.New(root, 0, []string{"secret.txt"})
	if err != nil {
		t.Fatal(err)
	}
	return fs, root
}

func params(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHostFSRespectsTheServerDenylist(t *testing.T) {
	fs, root := denylistFS(t)

	refused := []struct {
		name string
		call func() (any, error)
	}{
		{"read", func() (any, error) { return fsRead(fs, params(t, map[string]string{"path": "secret.txt"})) }},
		{"write", func() (any, error) {
			return fsWrite(fs, params(t, map[string]string{"path": "secret.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("x"))}))
		}},
		{"copy", func() (any, error) { return fsCopy(fs, params(t, map[string]string{"path": "secret.txt"})) }},
		{"chmod", func() (any, error) {
			return fsChmod(fs, params(t, map[string]any{"path": "secret.txt", "mode": 0o777}))
		}},
		{"rename from", func() (any, error) {
			return fsRename(fs, params(t, map[string]string{"from": "secret.txt", "to": "public.txt"}))
		}},
		{"rename to", func() (any, error) {
			return fsRename(fs, params(t, map[string]string{"from": "notes.txt", "to": "secret.txt"}))
		}},
		{"delete", func() (any, error) { return fsDelete(fs, params(t, map[string]string{"path": "secret.txt"})) }},
	}
	for _, tt := range refused {
		if _, err := tt.call(); err == nil {
			t.Errorf("fs.%s on a denylisted file should be refused", tt.name)
		}
	}

	b, err := os.ReadFile(filepath.Join(root, "secret.txt"))
	if err != nil || string(b) != "contents of secret.txt" {
		t.Fatalf("the denylisted file changed: %q %v", b, err)
	}
	if info, _ := os.Stat(filepath.Join(root, "secret.txt")); info.Mode().Perm() != 0o644 {
		t.Fatalf("the denylisted file's mode changed to %o", info.Mode().Perm())
	}

	// Deleting a directory leaves the denylisted file inside it in place.
	if _, err := fsDelete(fs, params(t, map[string]string{"path": "data"})); err != nil {
		t.Fatalf("deleting the directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "secret.txt")); err != nil {
		t.Fatalf("a recursive delete removed a denylisted file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "keep.txt")); !os.IsNotExist(err) {
		t.Fatalf("the rest of the directory should be gone, stat err = %v", err)
	}

	// Other files still work.
	res, err := fsRead(fs, params(t, map[string]string{"path": "notes.txt"}))
	if err != nil {
		t.Fatalf("reading an ordinary file: %v", err)
	}
	got, _ := base64.StdEncoding.DecodeString(res.(map[string]any)["content_base64"].(string))
	if string(got) != "contents of notes.txt" {
		t.Fatalf("read %q", got)
	}
}

func TestHostFSChmodTakesUnixModes(t *testing.T) {
	fs, root := denylistFS(t)
	if _, err := fsChmod(fs, params(t, map[string]any{"path": "notes.txt", "mode": 0o2750})); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("mode = %v, want setgid with 0750", info.Mode())
	}
}

func TestPublishPluginEventKeepsTheTopic(t *testing.T) {
	bus := events.NewBus()
	ch := make(chan []byte, 1)
	bus.On(ch)
	defer bus.Off(ch)

	publishPluginEvent(bus, "backup:started", map[string]int{"n": 1})

	select {
	case b := <-ch:
		var e struct {
			Topic string
			Data  struct {
				Topic string         `json:"topic"`
				Data  map[string]int `json:"data"`
			}
		}
		if err := json.Unmarshal(b, &e); err != nil {
			t.Fatal(err)
		}
		if e.Topic != "plugin" || e.Data.Topic != "backup:started" || e.Data.Data["n"] != 1 {
			t.Fatalf("event = %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event published")
	}
}
