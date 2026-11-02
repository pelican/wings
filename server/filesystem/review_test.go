package filesystem

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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
	if _, err := fs.root.Stat("link"); err != nil {
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
