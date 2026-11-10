package filesystem

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"emperror.dev/errors"
	ignore "github.com/sabhiram/go-gitignore"
)

// A leading "../" must not let a request get around an anchored denylist entry:
// "/../config.yml" and "../config.yml" both name config.yml in the server root.
func TestFilesystem_IsIgnoredBlocksParentPrefix(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.denylist = ignore.CompileIgnoreLines("/config.yml")
	for _, p := range []string{"../config.yml", "/../config.yml", "../../config.yml", "a/../../config.yml"} {
		if err := fs.IsIgnored(p); !IsErrorCode(err, ErrCodeDenylistFile) {
			t.Errorf("expected %q to be denied, got %v", p, err)
		}
	}
}

// Selecting "." compresses everything in the directory.
func TestFilesystem_CompressFilesWholeDirectory(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("a.txt", []byte("a"))
	if err := os.Mkdir(filepath.Join(fs.rootPath, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write("sub/b.txt", []byte("b"))

	info, _, err := fs.CompressFiles(context.Background(), "/", "", []string{"."}, "tar.gz")
	if err != nil {
		t.Fatalf("expected the directory to be compressed, got %v", err)
	}
	names := archiveNames(t, filepath.Join(fs.rootPath, info.Name()))
	if got, want := strings.Join(names, ","), "a.txt,sub,sub/b.txt"; got != want {
		t.Errorf("unexpected archive contents: got %s, want %s", got, want)
	}
}

// The default archive name only has one-second resolution. A second archive made
// in the same second, or a file the user already has with that name, must get a
// new name instead of failing (or, as main did, being overwritten).
func TestFilesystem_CompressFilesDefaultNameAlreadyTaken(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("a.txt", []byte("a"))
	now := time.Now()
	var taken []string
	for _, ts := range []time.Time{now, now.Add(time.Second)} {
		n := "archive-" + strings.ReplaceAll(ts.Format(time.RFC3339), ":", "") + ".tar.gz"
		fs.write(n, []byte("keep me"))
		taken = append(taken, n)
	}

	info, _, err := fs.CompressFiles(context.Background(), "/", "", []string{"a.txt"}, "tar.gz")
	if err != nil {
		t.Fatalf("expected the archive to get a free name, got %v", err)
	}
	for _, n := range taken {
		if info.Name() == n {
			t.Errorf("archive reused the existing name %s", n)
		}
		if b, _ := os.ReadFile(filepath.Join(fs.rootPath, n)); string(b) != "keep me" {
			t.Errorf("existing file %s was overwritten", n)
		}
	}
}

// Every archive format buffers its final bytes until it is closed. Running out
// of space at that point must fail the request, not return a truncated archive.
func TestFilesystem_CompressFilesReportsFailureOnFinalWrite(t *testing.T) {
	for _, ext := range []string{"zip", "tar.gz", "tar.bz2", "tar.xz"} {
		t.Run(ext, func(t *testing.T) {
			fs := NewFs()
			defer fs.reset()

			b := make([]byte, 200)
			_, _ = rand.Read(b)
			fs.write("small.bin", b)
			fs.SetDiskLimit(100)
			fs.quota.SetUsage(0)

			_, _, err := fs.CompressFiles(context.Background(), "/", "", []string{"small.bin"}, ext)
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
		})
	}
}

// Reading a named pipe as if it were a directory must fail straight away rather
// than block until something writes to the pipe.
func TestFilesystem_ReadDirOnNamedPipeDoesNotBlock(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	if err := syscall.Mkfifo(filepath.Join(fs.rootPath, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, fn := range map[string]func() error{
		"ReadDir":       func() error { _, err := fs.ReadDir("pipe"); return err },
		"ListDirectory": func() error { _, err := fs.ListDirectory("pipe"); return err },
	} {
		done := make(chan error, 1)
		go func() { done <- fn() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: expected an error for a named pipe", name)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s blocked on a named pipe", name)
		}
	}
}

// When a delete only partly succeeds, the space that was freed is no longer
// counted against the server.
func TestFilesystem_DeletePartialFailureReleasesFreedSpace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove files from read-only directories")
	}
	fs := NewFs()
	defer fs.reset()

	if err := os.MkdirAll(filepath.Join(fs.rootPath, "a/locked"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write("a/freed.bin", make([]byte, 4096))
	fs.write("a/locked/kept.bin", make([]byte, 4096))
	if err := os.Chmod(filepath.Join(fs.rootPath, "a/locked"), 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(fs.rootPath, "a/locked"), 0o755)

	before, err := fs.updateCachedDiskUsage()
	if err != nil {
		t.Fatal(err)
	}

	if err := fs.Delete("a"); err == nil {
		t.Fatal("expected the delete to fail on the read-only directory")
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "a/freed.bin")); !os.IsNotExist(err) {
		t.Fatalf("expected a/freed.bin to have been removed, got %v", err)
	}
	if got := fs.CachedUsage(); got > before-4096 {
		t.Errorf("expected usage to drop by at least 4096 from %d, got %d", before, got)
	}
}

// A link created inside a directory that is itself a symlink must not end up
// pointing somewhere it cannot be followed.
func TestFilesystem_SymlinkThroughSymlinkedParent(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("source.txt", []byte("hello"))
	if err := os.Symlink(".", filepath.Join(fs.rootPath, "alias")); err != nil {
		t.Fatal(err)
	}

	if err := fs.Symlink("source.txt", "alias/link"); err != nil {
		return // refusing is fine
	}
	if _, err := fs.root.Load().Stat("link"); err != nil {
		t.Fatalf("created a link that cannot be followed: %v", err)
	}
}

// failingWriter accepts the first n bytes and then fails, like a disk filling up.
type failingWriter struct{ n int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		k := w.n
		w.n = 0
		return k, syscall.ENOSPC
	}
	w.n -= len(p)
	return len(p), nil
}

// A backup small enough to sit entirely in the compressor's buffer is only
// written out when the stream is closed. A failure then must still be returned.
func TestArchive_StreamReportsFailureOnFinalWrite(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("small.txt", []byte("hello"))
	a, err := fs.NewArchive(".")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Stream(context.Background(), &failingWriter{n: 64}); err == nil {
		t.Fatal("expected the failed write to be reported")
	}
}

// Compressing "." keeps the leading dot of hidden files and folders.
func TestFilesystem_CompressFilesWholeDirectoryKeepsDotfiles(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write(".env", []byte("a"))
	fs.write("env", []byte("b"))
	if err := os.Mkdir(filepath.Join(fs.rootPath, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write(".hidden/x", []byte("c"))

	info, _, err := fs.CompressFiles(context.Background(), "/", "", []string{"."}, "tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	names := archiveNames(t, filepath.Join(fs.rootPath, info.Name()))
	if got, want := strings.Join(names, ","), ".env,.hidden,.hidden/x,env"; got != want {
		t.Errorf("unexpected archive contents: got %s, want %s", got, want)
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Single-file decompression must never truncate the archive it is reading.
func TestFilesystem_DecompressSingleFileNeverOverwritesItself(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	gz := gzipBytes(t, []byte("hello"))
	fs.write("data", gz)
	if err := fs.DecompressFile(context.Background(), "/", "data"); err == nil {
		t.Error("expected an error when the output would be the archive itself")
	}
	if b, _ := os.ReadFile(filepath.Join(fs.rootPath, "data")); !bytes.Equal(b, gz) {
		t.Errorf("the archive was changed: %d bytes left of %d", len(b), len(gz))
	}

	// An upper case extension is still stripped.
	fs.write("dump.sql.GZ", gz)
	if err := fs.DecompressFile(context.Background(), "/", "dump.sql.GZ"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(fs.rootPath, "dump.sql")); string(b) != "hello" {
		t.Errorf("expected dump.sql to contain hello, got %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(fs.rootPath, "dump.sql.GZ")); !bytes.Equal(b, gz) {
		t.Error("the archive was changed")
	}
}

// Chmod passes setgid and sticky through, which backup restore relies on.
func TestFilesystem_ChmodKeepsSetgidAndSticky(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	if err := os.Mkdir(filepath.Join(fs.rootPath, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.Chmod("shared", 0o775|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(fs.rootPath, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSetgid == 0 || st.Mode()&os.ModeSticky == 0 {
		t.Errorf("expected setgid and sticky, got %v", st.Mode())
	}
}

// tarGz returns a .tar.gz holding one file of the given size. Zeros compress to
// almost nothing, so the archive is tiny while its contents are not.
func tarGz(t *testing.T, name string, size int) []byte {
	t.Helper()
	var b bytes.Buffer
	gw := gzip.NewWriter(&b)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(size), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// An archive whose contents do not fit is refused before anything is written,
// as on main; one that fits is allowed.
func TestFilesystem_SpaceAvailableForDecompression(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("big.tar.gz", tarGz(t, "big.bin", 256*1024))
	fs.write("small.tar.gz", tarGz(t, "small.bin", 1024))
	fs.SetDiskLimit(64 * 1024)
	if _, err := fs.updateCachedDiskUsage(); err != nil {
		t.Fatal(err)
	}

	err := fs.SpaceAvailableForDecompression(context.Background(), "/", "big.tar.gz")
	if !IsErrorCode(err, ErrCodeDiskSpace) {
		t.Fatalf("expected ErrCodeDiskSpace for big.tar.gz, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "big.bin")); !os.IsNotExist(err) {
		t.Errorf("expected nothing to be written, got %v", err)
	}
	if err := fs.SpaceAvailableForDecompression(context.Background(), "/", "small.tar.gz"); err != nil {
		t.Errorf("expected small.tar.gz to fit, got %v", err)
	}

	fs.write("notes.txt", []byte("not an archive"))
	if err := fs.SpaceAvailableForDecompression(context.Background(), "/", "notes.txt"); !IsErrorCode(err, ErrCodeUnknownArchive) {
		t.Errorf("expected ErrCodeUnknownArchive for a text file, got %v", err)
	}
}

// Wings keeps the server folder open, but Docker mounts it by path. If the folder
// is moved or deleted and then made again while Wings runs (by hand, by a
// reinstall, or by mounting a disk over it), file operations must use the new
// folder. Before this, writes went to the old folder and backups came out empty.
func TestFilesystem_FollowsReplacedServerFolder(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	names := func() []string {
		entries, err := fs.ReadDir("/")
		if err != nil {
			t.Fatalf("read dir: %v", err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}

	fs.write("before.txt", []byte("before"))
	if err := os.Rename(fs.rootPath, fs.rootPath+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fs.rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	fs.checked.Store(0) // as if rootCheckInterval had passed
	if err := fs.Write("moved.txt", strings.NewReader("x"), 1, 0o644); err != nil {
		t.Fatalf("write after the folder was moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "moved.txt")); err != nil {
		t.Errorf("write did not land in the new folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath+".old", "moved.txt")); err == nil {
		t.Error("write landed in the moved-away folder")
	}
	if got := names(); len(got) != 1 || got[0] != "moved.txt" {
		t.Errorf("listing shows %v, want only moved.txt", got)
	}
	_ = os.RemoveAll(fs.rootPath + ".old")

	// While nothing is at the path, operations fail as on main, rather than
	// writing into a folder that no longer exists.
	if err := os.RemoveAll(fs.rootPath); err != nil {
		t.Fatal(err)
	}
	fs.checked.Store(0)
	if err := fs.Write("lost.txt", strings.NewReader("x"), 1, 0o644); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write with the folder deleted: got %v, want a not-exist error", err)
	}
	if err := os.Mkdir(fs.rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	fs.checked.Store(0)
	if err := fs.Write("recreated.txt", strings.NewReader("x"), 1, 0o644); err != nil {
		t.Fatalf("write after the folder was made again: %v", err)
	}
	if got := names(); len(got) != 1 || got[0] != "recreated.txt" {
		t.Errorf("listing shows %v, want only recreated.txt", got)
	}
}
