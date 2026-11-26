package filesystem

import (
	"os"
	"path/filepath"
	"testing"
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
