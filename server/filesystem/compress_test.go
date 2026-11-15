package filesystem

import (
	"context"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	. "github.com/franela/goblin"
	"github.com/mholt/archives"
	ignore "github.com/sabhiram/go-gitignore"
)

// Given an archive named test.{ext}, with the following file structure:
//
//	test/
//	|──inside/
//	|────finside.txt
//	|──outside.txt
//
// this test will ensure that it's being decompressed as expected
func TestFilesystem_DecompressFile(t *testing.T) {
	g := Goblin(t)
	fs := NewFs()

	g.Describe("Decompress", func() {
		for _, ext := range []string{"zip", "rar", "tar", "tar.gz"} {
			g.It("can decompress a "+ext, func() {
				// copy the file to the new FS
				c, err := os.ReadFile("./testdata/test." + ext)
				g.Assert(err).IsNil()
				fs.write("test."+ext, c)

				// decompress
				err = fs.DecompressFile(context.Background(), "/", "test."+ext)
				g.Assert(err).IsNil()

				// make sure everything is where it is supposed to be
				_, err = os.Stat(filepath.Join(fs.rootPath, "test/outside.txt"))
				g.Assert(err).IsNil()

				st, err := os.Stat(filepath.Join(fs.rootPath, "test/inside"))
				g.Assert(err).IsNil()
				g.Assert(st.IsDir()).IsTrue()

				_, err = os.Stat(filepath.Join(fs.rootPath, "test/inside/finside.txt"))
				g.Assert(err).IsNil()
			})
		}

		g.AfterEach(func() {
			fs.reset()
		})
	})
}

func archiveNames(t *testing.T, archivePath string) []string {
	t.Helper()
	genericFs, err := archives.FileSystem(context.Background(), archivePath, nil)
	if err != nil {
		t.Fatalf("failed to open archive: %v", err)
	}
	var names []string
	err = iofs.WalkDir(genericFs, ".", func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != "." {
			names = append(names, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk archive: %v", err)
	}
	sort.Strings(names)
	return names
}

func TestFilesystem_CompressFiles(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	if err := os.MkdirAll(filepath.Join(fs.rootPath, "data/world"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write("data/world/level.dat", []byte("level"))
	fs.write("data/server.jar", []byte("jar"))
	fs.write("../outside.txt", []byte("outside"))
	if err := os.Symlink("../../outside.txt", filepath.Join(fs.rootPath, "data/escape.txt")); err != nil {
		t.Fatal(err)
	}

	st, mime, err := fs.CompressFiles(context.Background(), "/data", "backup", []string{"world", "escape.txt"}, "tar.gz")
	if err != nil {
		t.Fatalf("CompressFiles returned an error: %v", err)
	}
	if mime != "application/gzip" {
		t.Errorf("expected application/gzip, got %s", mime)
	}
	// The requested name is used as-is when it is not already taken.
	if st.Name() != "backup.tar.gz" {
		t.Errorf("expected the archive to be named backup.tar.gz, got %s", st.Name())
	}
	if fs.CachedUsage() != st.Size() {
		t.Errorf("expected disk usage %d to match the archive size %d", fs.CachedUsage(), st.Size())
	}

	archivePath := filepath.Join(fs.rootPath, "data", st.Name())
	got := archiveNames(t, archivePath)
	want := []string{"escape.txt", "world", "world/level.dat"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("unexpected archive contents: got %v, want %v", got, want)
	}

	// The symlink is stored as a link, never as the contents of the file outside the root.
	gfs, err := archives.FileSystem(context.Background(), archivePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := iofs.ReadFile(gfs, "escape.txt"); err == nil && string(b) == "outside" {
		t.Error("the contents of a file outside the root were added to the archive")
	}

	// A second archive with the same name gets a copy suffix.
	st, _, err = fs.CompressFiles(context.Background(), "/data", "backup", []string{"server.jar"}, "tar.gz")
	if err != nil {
		t.Fatalf("CompressFiles returned an error: %v", err)
	}
	if st.Name() != "backup copy.tar.gz" {
		t.Errorf("expected the archive to be named \"backup copy.tar.gz\", got %q", st.Name())
	}
}

func TestFilesystem_CompressFilesRespectsDiskLimit(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	// Data that does not compress well, so the archive is larger than the limit.
	b := make([]byte, 64*1024)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	fs.write("big.bin", b)
	fs.SetDiskLimit(1024)
	fs.quota.SetUsage(0)

	_, _, err := fs.CompressFiles(context.Background(), "/", "", []string{"big.bin"}, "zip")
	if err == nil || err.Error() != ErrNoSpaceAvailable.Error() {
		t.Fatalf("expected ErrNoSpaceAvailable, got %v", err)
	}
	entries, err := os.ReadDir(fs.rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected the partial archive to be removed, found %d entries", len(entries))
	}
	if fs.CachedUsage() != 0 {
		t.Errorf("expected disk usage to be released, got %d", fs.CachedUsage())
	}
}

// Opening a named pipe for reading blocks until a writer appears. None of the
// functions that look at file contents should ever do that.
func TestFilesystem_NamedPipesDoNotBlock(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	if err := syscall.Mkfifo(filepath.Join(fs.rootPath, "pipe"), 0o644); err != nil {
		t.Skipf("unable to create a named pipe: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		if _, err := fs.Stat("pipe"); err != nil {
			t.Errorf("Stat returned an error: %v", err)
		}
		if _, _, err := fs.File("pipe"); !IsErrorCode(err, ErrCodeNotRegular) {
			t.Errorf("expected File to return ErrCodeNotRegular, got %v", err)
		}
		if _, err := fs.ListDirectory("/"); err != nil {
			t.Errorf("ListDirectory returned an error: %v", err)
		}
		if err := fs.Writefile("pipe", strings.NewReader("data")); !IsErrorCode(err, ErrCodeNotRegular) {
			t.Errorf("expected Writefile to return ErrCodeNotRegular, got %v", err)
		}
		f, err := os.Create(filepath.Join(fs.rootPath, "../pipe.tar.gz"))
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		a, _ := NewArchive(fs.root.Load(), ".")
		if err := a.Create(context.Background(), f); err != nil {
			t.Errorf("Archive.Create returned an error: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an operation on a named pipe blocked")
	}
}

func TestFilesystem_Chmod(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("file.txt", []byte("content"))
	if err := fs.Chmod("/file.txt", 0o600); err != nil {
		t.Fatalf("Chmod returned an error: %v", err)
	}
	st, err := os.Stat(filepath.Join(fs.rootPath, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("expected mode 0600, got %o", st.Mode().Perm())
	}
}

func TestFilesystem_ListDirectorySorting(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	for _, name := range []string{"b.txt", "a.txt", "c.txt"} {
		fs.write(name, []byte("content"))
	}
	for _, name := range []string{"zdir", "adir"} {
		if err := os.Mkdir(filepath.Join(fs.rootPath, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	out, err := fs.ListDirectory("/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range out {
		names = append(names, s.Name())
	}
	if got, want := strings.Join(names, ","), "adir,zdir,a.txt,b.txt,c.txt"; got != want {
		t.Errorf("unexpected order: got %s, want %s", got, want)
	}
}

func TestFilesystem_IsIgnoredCleansPaths(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.denylist = ignore.CompileIgnoreLines("/config.yml")
	for _, p := range []string{"config.yml", "/config.yml", "foo/../config.yml", "./config.yml"} {
		if err := fs.IsIgnored(p); !IsErrorCode(err, ErrCodeDenylistFile) {
			t.Errorf("expected %q to be denied, got %v", p, err)
		}
	}
	if err := fs.IsIgnored("foo/config.yml"); err != nil {
		t.Errorf("expected foo/config.yml to be allowed, got %v", err)
	}
}

func TestFilesystem_SafeDeleteRecursivelyKeepsDenylistedFiles(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.denylist = ignore.CompileIgnoreLines("config.yml")
	if err := os.MkdirAll(filepath.Join(fs.rootPath, "dir/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write("dir/config.yml", []byte("secret"))
	fs.write("dir/other.txt", []byte("other"))
	fs.write("dir/sub/file.txt", []byte("file"))
	fs.write("config.yml", []byte("secret"))

	if err := fs.SafeDeleteRecursively("dir"); err != nil {
		t.Fatalf("SafeDeleteRecursively returned an error: %v", err)
	}
	if err := fs.SafeDeleteRecursively("config.yml"); err != nil {
		t.Fatalf("SafeDeleteRecursively returned an error: %v", err)
	}

	for _, p := range []string{"dir/config.yml", "config.yml"} {
		if _, err := os.Stat(filepath.Join(fs.rootPath, p)); err != nil {
			t.Errorf("expected denylisted %s to be kept: %v", p, err)
		}
	}
	for _, p := range []string{"dir/other.txt", "dir/sub"} {
		if _, err := os.Lstat(filepath.Join(fs.rootPath, p)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted, got %v", p, err)
		}
	}
}

func TestArchive_IgnoreAllowsReincludingFiles(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	if err := os.MkdirAll(filepath.Join(fs.rootPath, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write("build/keep.txt", []byte("keep"))
	fs.write("build/drop.txt", []byte("drop"))
	fs.write("server.jar", []byte("jar"))

	archivePath := filepath.Join(fs.rootPath, "../archive.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	a, err := NewArchive(fs.root.Load(), ".", WithIgnored("build/\n!build/keep.txt\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Create(context.Background(), f); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(archiveNames(t, archivePath), ",")
	if want := "build,build/keep.txt,server.jar"; got != want {
		t.Errorf("unexpected archive contents: got %s, want %s", got, want)
	}
}
