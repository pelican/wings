package backup

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pelican/wings/config"
)

const (
	legacyTestServer = "ce6ee345-6729-4aed-8fed-c866c535a69d"
	legacyTestBackup = "11111111-1111-1111-1111-111111111111"
)

func setLegacyTestBackupDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System:              config.SystemConfiguration{BackupDirectory: dir},
	})
	return dir
}

func writeFile(t *testing.T, p string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLocateLocal_MigratesLegacyArchive(t *testing.T) {
	dir := setLegacyTestBackupDir(t)
	legacy := filepath.Join(dir, legacyTestBackup+".tar.gz")
	current := filepath.Join(dir, legacyTestServer, legacyTestBackup+".tar.gz")
	writeFile(t, legacy, "legacy archive")

	b, st, err := LocateLocal(nil, legacyTestBackup, legacyTestServer)
	if err != nil {
		t.Fatalf("expected legacy backup to be located, got %v", err)
	}
	if b.Path() != current {
		t.Errorf("expected path %q, got %q", current, b.Path())
	}
	if st.Size() != int64(len("legacy archive")) {
		t.Errorf("unexpected size %d", st.Size())
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected legacy archive to be moved, stat returned %v", err)
	}
	if got, _ := os.ReadFile(current); string(got) != "legacy archive" {
		t.Errorf("unexpected contents at new location %q", got)
	}

	// Deleting the backup from the Panel (e.g. rotation) now removes it.
	if err := b.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(current); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected backup to be removed, stat returned %v", err)
	}
}

// Concurrent requests for the same legacy backup must all find it, including the
// ones whose rename loses the race to another request.
func TestLocateLocal_ConcurrentMigration(t *testing.T) {
	for i := 0; i < 50; i++ {
		dir := setLegacyTestBackupDir(t)
		writeFile(t, filepath.Join(dir, legacyTestBackup+".tar.gz"), "legacy archive")

		const n = 8
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, n)
		for j := 0; j < n; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _, err := LocateLocal(nil, legacyTestBackup, legacyTestServer)
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("iteration %d: expected every concurrent request to locate the backup, got %v", i, err)
			}
		}
	}
}

func TestLocateLocal_PrefersCurrentLocation(t *testing.T) {
	dir := setLegacyTestBackupDir(t)
	legacy := filepath.Join(dir, legacyTestBackup+".tar.gz")
	current := filepath.Join(dir, legacyTestServer, legacyTestBackup+".tar.gz")
	writeFile(t, legacy, "legacy")
	writeFile(t, current, "current")

	b, _, err := LocateLocal(nil, legacyTestBackup, legacyTestServer)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(b.Path()); string(got) != "current" {
		t.Errorf("expected the current archive to be used, got %q", got)
	}
	if got, _ := os.ReadFile(legacy); string(got) != "legacy" {
		t.Errorf("expected the legacy archive to be left alone, got %q", got)
	}
}

func TestLocateLocal_MissingBackup(t *testing.T) {
	setLegacyTestBackupDir(t)
	if _, _, err := LocateLocal(nil, legacyTestBackup, legacyTestServer); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected a not exist error, got %v", err)
	}
}

func TestMigrateLegacy_IgnoresSymlinks(t *testing.T) {
	dir := setLegacyTestBackupDir(t)
	target := filepath.Join(t.TempDir(), "elsewhere.tar.gz")
	writeFile(t, target, "not a backup")
	if err := os.Symlink(target, filepath.Join(dir, legacyTestBackup+".tar.gz")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	if _, _, err := LocateLocal(nil, legacyTestBackup, legacyTestServer); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected a symlinked legacy archive to be ignored, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, legacyTestBackup+".tar.gz")); err != nil {
		t.Errorf("expected symlink to be left in place, got %v", err)
	}
}

func TestLegacyLocalArchives(t *testing.T) {
	dir := setLegacyTestBackupDir(t)
	writeFile(t, filepath.Join(dir, legacyTestBackup+".tar.gz"), "12345")
	writeFile(t, filepath.Join(dir, "22222222-2222-2222-2222-222222222222.tar.gz"), "123")
	// Not counted: archives in a server directory, non UUID names, other files.
	writeFile(t, filepath.Join(dir, legacyTestServer, "33333333-3333-3333-3333-333333333333.tar.gz"), "1")
	writeFile(t, filepath.Join(dir, "notes.tar.gz"), "1")
	writeFile(t, filepath.Join(dir, "README"), "1")

	count, size, err := LegacyLocalArchives()
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || size != 8 {
		t.Fatalf("expected 2 archives totalling 8 bytes, got %d and %d", count, size)
	}
}
