package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"io"
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
	if err := s.RestoreBackup(b, nil, false); err != nil {
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

// A restore that deletes the server's files first checks the whole backup, so
// a corrupt backup leaves the files in place.
func TestRestoreBackupCorruptArchiveKeepsFiles(t *testing.T) {
	serverDir := t.TempDir()
	cfg := &config.Configuration{
		AuthenticationToken: "abc",
		System: config.SystemConfiguration{
			RootDirectory:     filepath.Dir(serverDir),
			BackupDirectory:   t.TempDir(),
			DiskCheckInterval: 150,
		},
	}
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)

	// Random data does not compress, so the archive is large enough to cut in half.
	data := make([]byte, 256*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "world.dat"), data, 0o644); err != nil {
		t.Fatal(err)
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

	b := backup.NewLocal(restoreTestClient{}, "22222222-2222-4222-8222-222222222222", s.ID(), "")
	if _, err := b.Generate(context.Background(), s.Filesystem(), ""); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(b.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(b.Path(), st.Size()/2); err != nil {
		t.Fatal(err)
	}

	if err := s.RestoreBackup(b, nil, true); err == nil {
		t.Fatal("expected restoring a corrupt backup to fail")
	}
	got, err := os.ReadFile(filepath.Join(serverDir, "world.dat"))
	if err != nil {
		t.Fatalf("expected the server's files to be kept: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("world.dat was changed")
	}
}

// A remote backup is downloaded once, checked, and then restored from the copy
// on disk, which is removed afterwards.
func TestRestoreBackupRemoteVerifiesThenRestores(t *testing.T) {
	serverDir := t.TempDir()
	backupDir := t.TempDir()
	cfg := &config.Configuration{
		AuthenticationToken: "abc",
		System: config.SystemConfiguration{
			RootDirectory:     filepath.Dir(serverDir),
			BackupDirectory:   backupDir,
			DiskCheckInterval: 150,
		},
	}
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)

	if err := os.WriteFile(filepath.Join(serverDir, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
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

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: "new.txt", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	b := backup.NewS3(restoreTestClient{}, "33333333-3333-4333-8333-333333333333", s.ID(), "")
	if err := s.RestoreBackup(b, io.NopCloser(&buf), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(serverDir, "old.txt")); !os.IsNotExist(err) {
		t.Errorf("expected old.txt to be deleted, got %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(serverDir, "new.txt")); err != nil || string(got) != "new" {
		t.Errorf("expected new.txt to be restored, got %q, %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(backupDir, ".restore-*")); len(left) != 0 {
		t.Errorf("expected the temporary download to be removed, found %v", left)
	}
}
