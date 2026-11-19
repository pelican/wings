package filesystem

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// These cases answer the way main does.

func TestFilesystem_RenameToItselfDoesNothing(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("a.txt", []byte("a"))
	for _, to := range []string{"a.txt", "/a.txt", "./a.txt"} {
		if err := fs.Rename("a.txt", to); err != nil {
			t.Errorf("Rename(a.txt, %q): %v", to, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "a.txt")); err != nil {
		t.Errorf("a.txt is gone: %v", err)
	}
}

func TestFilesystem_CreateDirectoryDotDotUnderRoot(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	if err := fs.CreateDirectory("../name", "/"); err != nil {
		t.Fatalf("CreateDirectory(../name, /): %v", err)
	}
	if st, err := os.Stat(filepath.Join(fs.rootPath, "name")); err != nil || !st.IsDir() {
		t.Errorf("name was not created inside the server: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(fs.rootPath), "name")); err == nil {
		t.Error("name was created outside the server")
	}
	if err := fs.CreateDirectory("test", "e/../../something"); err == nil {
		t.Error("a relative path above the root must still fail")
	}
}

func TestFilesystem_CompressInMissingFolderIsNotExist(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	_, _, err := fs.CompressFiles(context.Background(), "missing", "", []string{"a.txt"}, "tar.gz")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected a not exist error, got %v", err)
	}
}

func TestFilesystem_CompressStoresPipesInTarOnly(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	if err := syscall.Mkfifo(filepath.Join(fs.rootPath, "pipe"), 0o644); err != nil {
		t.Skipf("unable to create a named pipe: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		info, _, err := fs.CompressFiles(context.Background(), "/", "sel", []string{"pipe"}, "tar.gz")
		if err != nil {
			t.Errorf("tar.gz: %v", err)
			return
		}
		f, err := os.Open(filepath.Join(fs.rootPath, info.Name()))
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Error(err)
			return
		}
		tr := tar.NewReader(gz)
		hdr, err := tr.Next()
		if err != nil {
			t.Errorf("reading the archive: %v", err)
			return
		}
		if hdr.Name != "pipe" || hdr.Typeflag != tar.TypeFifo {
			t.Errorf("got %q type %c, want pipe as a fifo", hdr.Name, hdr.Typeflag)
		}
		if _, err := tr.Next(); !errors.Is(err, io.EOF) {
			t.Errorf("expected one entry, got more: %v", err)
		}

		if _, _, err := fs.CompressFiles(context.Background(), "/", "sel", []string{"pipe"}, "zip"); err == nil {
			t.Error("a zip of only a pipe has nothing to store and should fail")
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("compressing a named pipe blocked")
	}
}

func TestFilesystem_DecompressOverItselfIsTypedError(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("data", gzipBytes(t, []byte("hello")))
	err := fs.DecompressFile(context.Background(), "/", "data")
	if !IsErrorCode(err, ErrCodeArchiveIsOutput) {
		t.Fatalf("expected ErrCodeArchiveIsOutput, got %v", err)
	}
	var fserr *Error
	if !errors.As(err, &fserr) || fserr.Resolved() != ".gz" {
		t.Errorf("expected the extension .gz, got %v", err)
	}
}
