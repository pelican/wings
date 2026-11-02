package filesystem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystem_WriteStream(t *testing.T) {
	t.Run("writes data of unknown size", func(t *testing.T) {
		fs, rfs := NewFs()
		defer fs.TruncateRootDirectory()

		if err := fs.WriteStream("dir/Dockerfile", strings.NewReader("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(rfs.root, "server", "dir/Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "FROM scratch\n" {
			t.Errorf("unexpected contents %q", b)
		}
	})

	t.Run("stays within the disk limit", func(t *testing.T) {
		fs, rfs := NewFs()
		defer fs.TruncateRootDirectory()
		fs.SetDiskLimit(10)
		fs.unixFS.SetUsage(0)

		if err := fs.WriteStream("fits.txt", strings.NewReader("0123456789"), 0o644); err != nil {
			t.Fatalf("expected data exactly the size of the limit to fit, got %v", err)
		}
		if usage := fs.CachedUsage(); usage != 10 {
			t.Errorf("expected usage 10, got %d", usage)
		}

		// Overwriting the same file frees its current size.
		if err := fs.WriteStream("fits.txt", strings.NewReader("abc"), 0o644); err != nil {
			t.Fatalf("expected overwrite to fit, got %v", err)
		}
		if usage := fs.CachedUsage(); usage != 3 {
			t.Errorf("expected usage 3, got %d", usage)
		}

		err := fs.WriteStream("too-big.txt", strings.NewReader(strings.Repeat("x", 100)), 0o644)
		if !IsErrorCode(err, ErrCodeDiskSpace) {
			t.Fatalf("expected disk space error, got %v", err)
		}
		if _, err := os.Stat(filepath.Join(rfs.root, "server", "too-big.txt")); !os.IsNotExist(err) {
			t.Errorf("expected partial file to be removed, got %v", err)
		}
		if usage := fs.CachedUsage(); usage != 3 {
			t.Errorf("expected usage to be restored to 3, got %d", usage)
		}
	})
}
