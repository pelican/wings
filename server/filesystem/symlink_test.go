package filesystem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A relative symlink target is relative to the directory the link is created in,
// which is what SFTP clients such as OpenSSH send.
func TestFilesystem_SymlinkRelativeToLinkDirectory(t *testing.T) {
	fs := NewFs()
	defer fs.TruncateRootDirectory()

	if err := fs.CreateDirectory("sl/sub", "/"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"sl/sub/target.txt", "renamed.txt"} {
		r := strings.NewReader("data\n")
		if err := fs.Write(p, r, r.Size(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		source, target, want string
	}{
		{"sub/target.txt", "sl/rel-link", "sub/target.txt"},
		{"target.txt", "sl/sub/sibling-link", "target.txt"},
		{"../renamed.txt", "sl/up-link", "../renamed.txt"},
		{"./sub/../sub/target.txt", "sl/clean-link", "sub/target.txt"},
		// Absolute sources are relative to the server root and stored relative to
		// the link's directory.
		{"/sl/sub/target.txt", "sl/abs-link", "sub/target.txt"},
		{"/renamed.txt", "sl/sub/root-link", "../../renamed.txt"},
	} {
		if err := fs.Symlink(tc.source, tc.target); err != nil {
			t.Errorf("Symlink(%q, %q): unexpected error: %v", tc.source, tc.target, err)
			continue
		}
		got, err := os.Readlink(filepath.Join(fs.rootPath, tc.target))
		if err != nil {
			t.Errorf("%s: %v", tc.target, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Symlink(%q, %q): stored %q, want %q", tc.source, tc.target, got, tc.want)
		}
		// The link must resolve to the intended file.
		if b, err := os.ReadFile(filepath.Join(fs.rootPath, tc.target)); err != nil || string(b) != "data\n" {
			t.Errorf("%s does not resolve to the intended file: %q, %v", tc.target, b, err)
		}
	}
}

func TestFilesystem_SymlinkRefusesEscapesAndMissingSources(t *testing.T) {
	fs := NewFs()
	defer fs.TruncateRootDirectory()

	if err := fs.CreateDirectory("sl", "/"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ source, target string }{
		{"../../outside", "sl/escape-rel"},
		{"../../../etc/passwd", "sl/escape-etc"},
		{"/etc/passwd", "sl/escape-abs"},
		{"nope", "sl/dangling"},
	} {
		if err := fs.Symlink(tc.source, tc.target); err == nil {
			t.Errorf("Symlink(%q, %q): expected an error", tc.source, tc.target)
		}
		if _, err := os.Lstat(filepath.Join(fs.rootPath, tc.target)); err == nil {
			t.Errorf("Symlink(%q, %q): link was created", tc.source, tc.target)
		}
	}
}
