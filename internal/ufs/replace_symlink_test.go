//go:build unix

package ufs_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// entries returns the names in dir, failing the test on error.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(des))
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

func TestUnixFS_ReplaceWithSymlink(t *testing.T) {
	fs, err := newTestUnixFS()
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Cleanup()

	t.Run("replaces a regular file", func(t *testing.T) {
		p := filepath.Join(fs.Root, "unix_args.txt")
		if err := os.WriteFile(p, []byte("old contents"), 0o644); err != nil {
			t.Fatal(err)
		}
		previous, err := fs.ReplaceWithSymlink("libraries/unix_args.txt", "unix_args.txt", -1, -1)
		if err != nil {
			t.Fatal(err)
		}
		if previous == nil || !previous.Mode().IsRegular() || previous.Size() != int64(len("old contents")) {
			t.Fatalf("expected the replaced regular file to be returned, got %v", previous)
		}
		if got, err := os.Readlink(p); err != nil || got != "libraries/unix_args.txt" {
			t.Fatalf("expected a symlink to libraries/unix_args.txt, got %q, %v", got, err)
		}
	})

	t.Run("creates a missing entry", func(t *testing.T) {
		previous, err := fs.ReplaceWithSymlink("target", "new-link", -1, -1)
		if err != nil {
			t.Fatal(err)
		}
		if previous != nil {
			t.Fatalf("expected no replaced entry, got %v", previous)
		}
		if got, _ := os.Readlink(filepath.Join(fs.Root, "new-link")); got != "target" {
			t.Fatalf("unexpected link target %q", got)
		}
	})

	t.Run("keeps the existing file when the link cannot be created", func(t *testing.T) {
		p := filepath.Join(fs.Root, "keep.txt")
		if err := os.WriteFile(p, []byte("keep me"), 0o644); err != nil {
			t.Fatal(err)
		}
		// A target longer than PATH_MAX makes symlinkat fail.
		if _, err := fs.ReplaceWithSymlink(strings.Repeat("a", 5000), "keep.txt", -1, -1); err == nil {
			t.Fatal("expected an error for an over-long link target")
		}
		if b, err := os.ReadFile(p); err != nil || string(b) != "keep me" {
			t.Fatalf("expected the original file to be untouched, got %q, %v", b, err)
		}
	})

	t.Run("never replaces a directory", func(t *testing.T) {
		d := filepath.Join(fs.Root, "dir")
		if err := os.MkdirAll(filepath.Join(d, "child"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := fs.ReplaceWithSymlink("elsewhere", "dir", -1, -1); err == nil {
			t.Fatal("expected an error when replacing a directory")
		}
		if st, err := os.Lstat(filepath.Join(d, "child")); err != nil || !st.IsDir() {
			t.Fatalf("expected the directory to be untouched, got %v", err)
		}
	})

	t.Run("sets the owner of the link", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("changing ownership requires root")
		}
		if _, err := fs.ReplaceWithSymlink("target", "owned-link", 1234, 1235); err != nil {
			t.Fatal(err)
		}
		st, err := os.Lstat(filepath.Join(fs.Root, "owned-link"))
		if err != nil {
			t.Fatal(err)
		}
		sys := st.Sys().(*syscall.Stat_t)
		if sys.Uid != 1234 || sys.Gid != 1235 {
			t.Fatalf("expected owner 1234:1235, got %d:%d", sys.Uid, sys.Gid)
		}
	})

	// No temporary links are left behind by any of the cases above.
	for _, name := range entries(t, fs.Root) {
		if strings.Contains(name, ".wings-") {
			t.Errorf("temporary link %q was left behind", name)
		}
	}
}
