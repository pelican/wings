package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server/backup"
	"github.com/pelican/wings/server/filesystem"
)

// restoreTestClient accepts the restoration status that RestoreBackup reports.
type restoreTestClient struct{ remote.Client }

func (restoreTestClient) SendRestorationStatus(context.Context, string, bool) error { return nil }

// A local backup keeps the symlinks in the server directory, and restoring it
// after every file was deleted recreates them (#168).
func TestRestoreBackupRecreatesSymlinks(t *testing.T) {
	serverDir := t.TempDir()
	cfg := &config.Configuration{
		AuthenticationToken: "abc",
		System: config.SystemConfiguration{
			RootDirectory:     filepath.Dir(serverDir),
			BackupDirectory:   t.TempDir(),
			DiskCheckInterval: 150,
		},
	}
	// Restored files are chowned to the Wings user, so use the user running the
	// test, which is not root in CI.
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)

	// The NeoForge layout: unix_args.txt in the server root links into libraries.
	lib := filepath.Join(serverDir, "libraries", "neoforge")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "unix_args.txt"), []byte("-Xmx4G\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"unix_args.txt":               "libraries/neoforge/unix_args.txt",
		"libraries/neoforge/link.txt": "../../unix_args.txt",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(serverDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	s, err := New(restoreTestClient{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)
	s.cfg.Uuid = "ce6ee345-6729-4aed-8fed-c866c535a69d"
	env := &terminateTestEnvironment{}
	env.state.Store(environment.ProcessOfflineState)
	s.Environment = env
	s.fs, err = filesystem.New(serverDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.fs.Close() })

	b := backup.NewLocal(restoreTestClient{}, "11111111-1111-4111-8111-111111111111", s.ID(), "")
	if _, err := b.Generate(context.Background(), s.Filesystem(), ""); err != nil {
		t.Fatal(err)
	}

	// Restore with "delete all files".
	des, err := os.ReadDir(serverDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range des {
		if err := os.RemoveAll(filepath.Join(serverDir, de.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RestoreBackup(b, nil); err != nil {
		t.Fatal(err)
	}

	for name, target := range links {
		got, err := os.Readlink(filepath.Join(serverDir, name))
		if err != nil {
			t.Fatalf("expected %q to be restored as a symlink: %v", name, err)
		}
		if got != target {
			t.Errorf("%q: expected link target %q, got %q", name, target, got)
		}
	}
	content, err := os.ReadFile(filepath.Join(serverDir, "unix_args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "-Xmx4G\n" {
		t.Errorf("expected unix_args.txt to resolve to the library file, got %q", content)
	}
}
