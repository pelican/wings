package filesystem

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	ignore "github.com/sabhiram/go-gitignore"
)

func (tfs *testFs) setDenylist(lines ...string) {
	tfs.denylist = ignore.CompileIgnoreLines(lines...)
	tfs.hasDenylist = len(lines) > 0
}

func (tfs *testFs) symlink(target, link string) {
	p := filepath.Join(tfs.rootPath, link)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		panic(err)
	}
	if err := os.Symlink(target, p); err != nil {
		panic(err)
	}
}

// A symlink inside the server must not give access to a denylisted file. os.Root
// follows links that stay inside the root, so the check has to follow them too.
func TestFilesystem_IsIgnoredFollowsSymlinks(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/config.yml", "/secret/")
	fs.write("config.yml", []byte("token: abc"))
	if err := os.Mkdir(filepath.Join(fs.rootPath, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write("secret/key", []byte("k"))
	fs.write("notes.txt", []byte("n"))
	fs.symlink("config.yml", "sneaky")
	fs.symlink("../config.yml", "sub/sneaky")
	fs.symlink("sneaky", "chain")
	fs.symlink(".", "here")
	fs.symlink("secret", "s")
	fs.symlink("notes.txt", "fine")

	for _, p := range []string{"sneaky", "/sneaky", "sub/sneaky", "chain", "here/config.yml", "here/here/sneaky", "s/key", "here/secret/key"} {
		if err := fs.IsIgnored(p); !IsErrorCode(err, ErrCodeDenylistFile) {
			t.Errorf("expected %q to be denied, got %v", p, err)
		}
	}
	for _, p := range []string{"fine", "notes.txt", "here/notes.txt", "missing/file", "here/missing"} {
		if err := fs.IsIgnored(p); err != nil {
			t.Errorf("expected %q to be allowed, got %v", p, err)
		}
	}

	// A link made after an earlier check is caught on the next one.
	if err := fs.IsIgnored("later"); err != nil {
		t.Fatal(err)
	}
	fs.symlink("config.yml", "later")
	if err := fs.IsIgnored("later"); !IsErrorCode(err, ErrCodeDenylistFile) {
		t.Errorf("expected a link created after the first check to be denied, got %v", err)
	}
}

// A loop of links must not hang the check.
func TestFilesystem_IsIgnoredSymlinkLoop(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/config.yml")
	fs.symlink("b", "a")
	fs.symlink("a", "b")
	if err := fs.IsIgnored("a/x"); err != nil {
		t.Errorf("expected a symlink loop to be allowed through to os.Root, got %v", err)
	}
}

// Deleting or renaming a link only touches the link, so a link to a denylisted
// file can be removed. Going through a symlinked directory is still denied.
func TestFilesystem_IsIgnoredNoFollow(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/config.yml")
	fs.write("config.yml", []byte("token: abc"))
	fs.symlink("config.yml", "sneaky")
	fs.symlink(".", "here")

	if err := fs.IsIgnoredNoFollow("sneaky"); err != nil {
		t.Errorf("expected the link itself to be allowed, got %v", err)
	}
	for _, p := range []string{"config.yml", "here/config.yml", "../config.yml"} {
		if err := fs.IsIgnoredNoFollow(p); !IsErrorCode(err, ErrCodeDenylistFile) {
			t.Errorf("expected %q to be denied, got %v", p, err)
		}
	}

	if err := fs.SafeDeleteRecursively("sneaky"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(fs.rootPath, "sneaky")); !os.IsNotExist(err) {
		t.Errorf("expected the link to be deleted, got %v", err)
	}
	if err := fs.SafeDeleteRecursively("here/config.yml"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "config.yml")); err != nil {
		t.Errorf("expected config.yml to be left in place, got %v", err)
	}
}

// Chmod through a link must not change a denylisted file.
func TestFilesystem_ChmodThroughSymlinkToDenylistedFile(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/config.yml")
	fs.write("config.yml", []byte("token: abc"))
	if err := os.Chmod(filepath.Join(fs.rootPath, "config.yml"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs.symlink("config.yml", "sneaky")

	if err := fs.Chmod("sneaky", 0o777); !IsErrorCode(err, ErrCodeDenylistFile) {
		t.Errorf("expected chmod through the link to be denied, got %v", err)
	}
	st, err := os.Stat(filepath.Join(fs.rootPath, "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("expected config.yml to keep 0600, got %o", st.Mode().Perm())
	}
}

// An archive must not be able to plant a link to a denylisted file, nor write
// through such a link with a later entry of the same name.
func TestFilesystem_DecompressSkipsLinksToDenylistedFiles(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.setDenylist("/config.yml")
	fs.write("config.yml", []byte("token: abc"))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range []*tar.Header{
		{Name: "sneaky", Typeflag: tar.TypeSymlink, Linkname: "config.yml", Mode: 0o777},
		{Name: "dir/sneaky", Typeflag: tar.TypeSymlink, Linkname: "../config.yml", Mode: 0o777},
		{Name: "ok", Typeflag: tar.TypeSymlink, Linkname: "data.txt", Mode: 0o777},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("overwritten")
	if err := tw.WriteHeader(&tar.Header{Name: "sneaky", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	fs.write("planted.tar.gz", buf.Bytes())

	if err := fs.DecompressFile(context.Background(), "/", "planted.tar.gz"); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"sneaky", "dir/sneaky"} {
		if st, err := os.Lstat(filepath.Join(fs.rootPath, p)); err == nil && st.Mode()&os.ModeSymlink != 0 {
			t.Errorf("expected no link at %s", p)
		}
	}
	if st, err := os.Lstat(filepath.Join(fs.rootPath, "ok")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("expected the link to an allowed file to be created, got %v", err)
	}
	b, err := os.ReadFile(filepath.Join(fs.rootPath, "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "token: abc" {
		t.Errorf("expected config.yml to be unchanged, got %q", b)
	}
}

// The Panel sends modes such as "4755"; setuid, setgid and sticky must reach the
// file. os.FileMode(0o4755) would silently lose them.
func TestFileModeFromUnix(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want os.FileMode
	}{
		{0o644, 0o644},
		{0o1777, os.ModeSticky | 0o777},
		{0o2755, os.ModeSetgid | 0o755},
		{0o4755, os.ModeSetuid | 0o755},
		{0o6755, os.ModeSetuid | os.ModeSetgid | 0o755},
		{0o7777, os.ModeSetuid | os.ModeSetgid | os.ModeSticky | 0o777},
		{0o17644, os.ModeSetuid | os.ModeSetgid | os.ModeSticky | 0o644},
	} {
		if got := FileModeFromUnix(tc.in); got != tc.want {
			t.Errorf("FileModeFromUnix(%o) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestFilesystem_ChmodAppliesSpecialBitsFromUnixMode(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("run.sh", []byte("#!/bin/sh"))
	if err := os.Mkdir(filepath.Join(fs.rootPath, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path string
		unix uint64
		want os.FileMode
	}{
		{"run.sh", 0o4755, os.ModeSetuid | 0o755},
		{"run.sh", 0o6755, os.ModeSetuid | os.ModeSetgid | 0o755},
		{"run.sh", 0o755, 0o755},
		{"shared", 0o1777, os.ModeSticky | 0o777},
		{"shared", 0o2775, os.ModeSetgid | 0o775},
	} {
		if err := fs.Chmod(tc.path, FileModeFromUnix(tc.unix)); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(filepath.Join(fs.rootPath, tc.path))
		if err != nil {
			t.Fatal(err)
		}
		got := st.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
		if got != tc.want {
			t.Errorf("chmod %s %o: got %v, want %v", tc.path, tc.unix, got, tc.want)
		}
	}
}

// os.Root applies ".." in a link's target after following the links before it,
// so "alias -> jump/../config.yml" with "jump -> sub/x" reaches sub/config.yml.
// The denylist check must resolve it the same way, both when reading and when an
// archive tries to plant such a link.
func TestFilesystem_IsIgnoredDotDotAfterSymlink(t *testing.T) {
	fs := NewFs()
	defer fs.reset()
	fs.setDenylist("/sub/config.yml")

	if err := os.MkdirAll(filepath.Join(fs.rootPath, "sub/x"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write("sub/config.yml", []byte("secret"))
	fs.symlink("sub/x", "jump")
	fs.symlink("jump/../config.yml", "alias")

	r, err := fs.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := r.ReadFile("alias"); err != nil || string(b) != "secret" {
		t.Fatalf("test setup: os.Root should reach sub/config.yml through alias, got %q, %v", b, err)
	}
	if got := fs.resolveInRoot("alias"); got != "sub/config.yml" {
		t.Errorf("resolveInRoot(alias) = %q, want sub/config.yml", got)
	}
	if err := fs.IsIgnored("alias"); !IsErrorCode(err, ErrCodeDenylistFile) {
		t.Errorf("IsIgnored(alias) = %v, want a denylist error", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "planted", Typeflag: tar.TypeSymlink, Linkname: "jump/../config.yml", Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	if err := fs.ExtractStreamUnsafe(context.Background(), "/", &buf); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fs.rootPath, "planted")); err == nil {
		t.Error("decompression kept a link that reaches a denylisted file")
	}
}

// openRoot reopens the server folder when the open one has been deleted, even if
// the new folder got the same inode number. isUnlinked is what tells them apart.
func TestIsUnlinked(t *testing.T) {
	// macOS doesn't report a link count of 0 for a removed folder that is still
	// open, so there openRoot only has SameFile to go on. Wings runs on Linux.
	if runtime.GOOS != "linux" {
		t.Skip("link count of a removed open folder is only reliable on Linux")
	}
	d := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(d)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	st, err := r.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	if isUnlinked(st) {
		t.Error("a folder that still exists is reported as unlinked")
	}
	if err := os.Remove(d); err != nil {
		t.Fatal(err)
	}
	if st, err = r.Stat("."); err != nil {
		t.Fatal(err)
	}
	if !isUnlinked(st) {
		t.Error("a removed folder that is still open is not reported as unlinked")
	}
}
