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
	fs := NewFs()
	defer fs.TruncateRootDirectory()

	if err := fs.CreateDirectory("libraries/neoforge", "/"); err != nil {
		t.Fatal(err)
	}
	r := strings.NewReader("-Xmx4G\n")
	if err := fs.Write("libraries/neoforge/unix_args.txt", r, r.Size(), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink in the server root, and one nested in a subdirectory. Symlink
	// stores links relative to the directory they are in.
	if err := fs.Symlink("libraries/neoforge/unix_args.txt", "unix_args.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Symlink("/unix_args.txt", "libraries/neoforge/link.txt"); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	a, err := fs.NewArchive(".")
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Create(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	headers := readTarHeaders(t, archivePath)
	for name, target := range map[string]string{
		"unix_args.txt":               "libraries/neoforge/unix_args.txt",
		"libraries/neoforge/link.txt": "../../unix_args.txt",
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
	fs := NewFs()
	defer fs.TruncateRootDirectory()

	// Missing parent directories are created.
	if err := fs.RestoreSymlink("a/b/link", "../target.txt"); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(fs.rootPath, "a/b/link"))
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
	st, err := os.Lstat(filepath.Join(fs.rootPath, "unix_args.txt"))
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

	// No temporary links are left behind.
	des, err := os.ReadDir(fs.rootPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range des {
		if strings.Contains(de.Name(), ".wings-") {
			t.Errorf("temporary link %q was left behind", de.Name())
		}
	}
}

// A link restored through a symlinked parent directory is refused without
// creating any directories at the parent link's target.
func TestFilesystem_RestoreSymlinkThroughSymlinkedParent(t *testing.T) {
	fs := NewFs()
	defer fs.TruncateRootDirectory()

	if err := fs.CreateDirectory("real", "/"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(fs.rootPath, "alias")); err != nil {
		t.Fatal(err)
	}

	if err := fs.RestoreSymlink("alias/new/link", "target.txt"); err == nil {
		t.Fatal("expected an error when restoring a link through a symlinked directory")
	}
	if _, err := os.Lstat(filepath.Join(fs.rootPath, "real", "new")); !os.IsNotExist(err) {
		t.Errorf("expected real/new not to be created, got err %v", err)
	}

	if err := fs.RestoreSymlink("alias/link", "target.txt"); err == nil {
		t.Error("expected an error when restoring a link directly inside a symlinked directory")
	}
	if _, err := os.Lstat(filepath.Join(fs.rootPath, "real", "link")); !os.IsNotExist(err) {
		t.Errorf("expected real/link not to be created, got err %v", err)
	}
}

// Replacing a regular file with a link releases its size from the disk usage.
func TestFilesystem_RestoreSymlinkQuota(t *testing.T) {
	fs := NewFs()
	defer fs.TruncateRootDirectory()
	fs.SetDiskLimit(1024)
	fs.quota.SetUsage(0)

	r := strings.NewReader("0123456789")
	if err := fs.Write("unix_args.txt", r, r.Size(), 0o644); err != nil {
		t.Fatal(err)
	}
	if usage := fs.CachedUsage(); usage != 10 {
		t.Fatalf("expected usage 10 after writing the file, got %d", usage)
	}

	if err := fs.RestoreSymlink("unix_args.txt", "libraries/unix_args.txt"); err != nil {
		t.Fatal(err)
	}
	if usage := fs.CachedUsage(); usage != 0 {
		t.Errorf("expected usage 0 after replacing the file with a link, got %d", usage)
	}

	// Replacing a link with another link does not change the usage.
	if err := fs.RestoreSymlink("unix_args.txt", "other.txt"); err != nil {
		t.Fatal(err)
	}
	if usage := fs.CachedUsage(); usage != 0 {
		t.Errorf("expected usage to stay 0, got %d", usage)
	}
}

func keys(m map[string]*tar.Header) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
