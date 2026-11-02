package filesystem

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readTarHeaders returns every header in the gzipped tar archive at p, keyed by name.
func readTarHeaders(t *testing.T, p string) map[string]*tar.Header {
	t.Helper()

	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()

	headers := make(map[string]*tar.Header)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		headers[h.Name] = h
	}
	return headers
}

func TestArchive_IncludesSymlinks(t *testing.T) {
	fs, rfs := NewFs()
	defer fs.TruncateRootDirectory()

	if err := fs.CreateDirectory("libraries/neoforge", "/"); err != nil {
		t.Fatal(err)
	}
	r := strings.NewReader("-Xmx4G\n")
	if err := fs.Write("libraries/neoforge/unix_args.txt", r, r.Size(), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink in the server root, and one nested in a subdirectory.
	if err := fs.Symlink("libraries/neoforge/unix_args.txt", "unix_args.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Symlink("unix_args.txt", "libraries/neoforge/link.txt"); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(rfs.root, "archive.tar.gz")
	a := &Archive{Filesystem: fs}
	if err := a.Create(context.Background(), archivePath); err != nil {
		t.Fatal(err)
	}

	headers := readTarHeaders(t, archivePath)
	for name, target := range map[string]string{
		"unix_args.txt":               "libraries/neoforge/unix_args.txt",
		"libraries/neoforge/link.txt": "unix_args.txt",
	} {
		h, ok := headers[name]
		if !ok {
			t.Fatalf("expected symlink %q in archive, got entries %v", name, keys(headers))
		}
		if h.Typeflag != tar.TypeSymlink {
			t.Errorf("%q: expected symlink typeflag, got %v", name, h.Typeflag)
		}
		if h.Linkname != target {
			t.Errorf("%q: expected link target %q, got %q", name, target, h.Linkname)
		}
	}
	if _, ok := headers["libraries/neoforge/unix_args.txt"]; !ok {
		t.Errorf("expected regular file in archive, got entries %v", keys(headers))
	}
}

func TestFilesystem_RestoreSymlink(t *testing.T) {
	fs, rfs := NewFs()
	defer fs.TruncateRootDirectory()

	// Missing parent directories are created.
	if err := fs.RestoreSymlink("a/b/link", "../target.txt"); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(rfs.root, "server", "a/b/link"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "../target.txt" {
		t.Errorf("expected target ../target.txt, got %q", got)
	}

	// An existing regular file is replaced by the link.
	r := strings.NewReader("")
	if err := fs.Write("unix_args.txt", r, 0, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.RestoreSymlink("unix_args.txt", "libraries/unix_args.txt"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(filepath.Join(rfs.root, "server", "unix_args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("expected unix_args.txt to be a symlink, got mode %v", st.Mode())
	}

	// A directory is never replaced.
	if err := fs.CreateDirectory("dir", "/"); err != nil {
		t.Fatal(err)
	}
	if err := fs.RestoreSymlink("dir", "elsewhere"); err == nil {
		t.Error("expected an error when replacing a directory with a symlink")
	}
}

func keys(m map[string]*tar.Header) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
