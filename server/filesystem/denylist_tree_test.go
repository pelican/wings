package filesystem

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Renaming a directory must not move a denylisted file out of its protected
// path (GHSA-v7qq-rmfm-47m3), nor move a file onto a protected path.
func TestFilesystem_IsIgnoredTreeContents(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/plugins/secret.yml", "config.yml")
	fs.writeAll("plugins/secret.yml", []byte("token: abc"))
	fs.writeAll("plugins/ok.txt", []byte("ok"))
	fs.writeAll("a/b/config.yml", []byte("x"))
	fs.writeAll("other/file.txt", []byte("x"))
	fs.symlink(".", "here")

	for _, c := range [][2]string{
		{"plugins", "p2"},
		{"/plugins", "/p2"},
		{"here/plugins", "p2"},
		{"a", "a2"},
		{"a/b", "c"},
	} {
		if err := fs.IsIgnoredTree(c[0], c[1]); !IsErrorCode(err, ErrCodeDenylistFile) {
			t.Errorf("IsIgnoredTree(%q, %q) = %v, want a denylist error", c[0], c[1], err)
		}
	}
	for _, c := range [][2]string{
		{"other", "o2"},
		{"plugins/ok.txt", "ok.txt"},
		{"missing", "m2"},
	} {
		if err := fs.IsIgnoredTree(c[0], c[1]); err != nil {
			t.Errorf("IsIgnoredTree(%q, %q) = %v, want nil", c[0], c[1], err)
		}
	}
}

func TestFilesystem_IsIgnoredTreeDestination(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/plugins/secret.yml")
	fs.writeAll("evil/secret.yml", []byte("planted"))
	fs.writeAll("fine/other.yml", []byte("x"))

	if err := fs.IsIgnoredTree("evil", "plugins"); !IsErrorCode(err, ErrCodeDenylistFile) {
		t.Errorf("expected moving evil/ onto plugins/ to be denied, got %v", err)
	}
	if err := fs.IsIgnoredTree("fine", "plugins"); err != nil {
		t.Errorf("expected moving fine/ onto plugins/ to be allowed, got %v", err)
	}
}

// Links are moved, not followed: a directory holding a link to a denylisted
// file, or a link to a directory holding one, can be renamed.
func TestFilesystem_IsIgnoredTreeDoesNotFollowLinks(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/plugins/secret.yml")
	fs.writeAll("plugins/secret.yml", []byte("token: abc"))
	fs.symlink("../plugins/secret.yml", "links/s")
	fs.symlink("plugins", "alias")

	if err := fs.IsIgnoredTree("links", "links2"); err != nil {
		t.Errorf("expected a directory of links to be renamable, got %v", err)
	}
	if err := fs.IsIgnoredTree("alias", "alias2"); err != nil {
		t.Errorf("expected a link to a directory to be renamable, got %v", err)
	}
}

func TestFilesystem_IsIgnoredTreeWithoutDenylist(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.writeAll("plugins/secret.yml", []byte("token: abc"))
	if err := fs.IsIgnoredTree("plugins", "p2"); err != nil {
		t.Errorf("expected no error without a denylist, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "plugins/secret.yml")); err != nil {
		t.Fatal(err)
	}
}

// writeAll is write, creating any parent directories first.
func (tfs *testFs) writeAll(name string, data []byte) {
	if err := os.MkdirAll(filepath.Join(tfs.rootPath, filepath.Dir(name)), 0o755); err != nil {
		panic(err)
	}
	tfs.write(name, data)
}

// A file written into a directory after CheckedRename checked it, but before
// the directory was moved, must not end up at a denylisted path.
func TestFilesystem_CheckedRenameUndoesPlantedFile(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/plugins/secret.yml")
	if err := os.Mkdir(filepath.Join(fs.rootPath, "staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	afterRenameCheck = func() {
		afterRenameCheck = nil
		fs.writeAll("staging/secret.yml", []byte("planted"))
	}
	defer func() { afterRenameCheck = nil }()

	if err := fs.CheckedRename("staging", "plugins"); !IsErrorCode(err, ErrCodeDenylistFile) {
		t.Fatalf("expected a denylist error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "staging/secret.yml")); err != nil {
		t.Errorf("expected the rename to be undone, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fs.rootPath, "plugins")); !os.IsNotExist(err) {
		t.Errorf("expected plugins/ not to exist, got %v", err)
	}
}

// A rename that would fill a directory another rename has already checked
// waits until that rename is done.
func TestFilesystem_CheckedRenameIsSerialized(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/plugins/secret.yml")
	if err := os.Mkdir(filepath.Join(fs.rootPath, "staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.writeAll("evil/secret.yml", []byte("planted"))

	done := make(chan error, 1)
	afterRenameCheck = func() {
		afterRenameCheck = nil
		go func() { done <- fs.CheckedRename("evil/secret.yml", "staging/secret.yml") }()
		select {
		case err := <-done:
			t.Fatalf("expected the second rename to wait, it returned %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() { afterRenameCheck = nil }()

	if err := fs.CheckedRename("staging", "plugins"); err != nil {
		t.Fatalf("expected renaming the empty staging/ to succeed, got %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("expected the second rename to succeed, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fs.rootPath, "plugins/secret.yml")); !os.IsNotExist(err) {
		t.Errorf("expected plugins/secret.yml not to exist, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "staging/secret.yml")); err != nil {
		t.Errorf("expected the file at staging/secret.yml, got %v", err)
	}
}

func TestFilesystem_CheckedRenameMovesDirectory(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/plugins/secret.yml")
	fs.writeAll("other/file.txt", []byte("x"))
	if err := fs.CheckedRename("other", "moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "moved/file.txt")); err != nil {
		t.Errorf("expected moved/file.txt, got %v", err)
	}
}

// If the source is recreated while a rename that has to be undone is under
// way, the moved tree is put beside it rather than left at the protected path.
func TestFilesystem_CheckedRenameUndoesWhenSourceRecreated(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/plugins/secret.yml")
	if err := os.Mkdir(filepath.Join(fs.rootPath, "staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.writeAll("staging.restored-1/taken", []byte("x"))
	afterRenameCheck = func() {
		afterRenameCheck = nil
		fs.writeAll("staging/secret.yml", []byte("planted"))
	}
	afterRenameMove = func() {
		afterRenameMove = nil
		fs.writeAll("staging/new.txt", []byte("recreated"))
	}
	defer func() { afterRenameCheck, afterRenameMove = nil, nil }()

	if err := fs.CheckedRename("staging", "plugins"); !IsErrorCode(err, ErrCodeDenylistFile) {
		t.Fatalf("expected a denylist error, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fs.rootPath, "plugins")); !os.IsNotExist(err) {
		t.Errorf("expected plugins/ not to exist, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "staging/new.txt")); err != nil {
		t.Errorf("expected the recreated staging/ to be left alone, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "staging.restored-2/secret.yml")); err != nil {
		t.Errorf("expected the moved tree at staging.restored-2, got %v", err)
	}
}
