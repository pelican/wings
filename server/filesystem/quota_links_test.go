package filesystem

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newQuotaLinkTestFs returns a filesystem with a disk limit whose usage is
// tracked from a fresh scan.
func newQuotaLinkTestFs(t *testing.T, limit int64) (*Filesystem, string) {
	t.Helper()
	tfs := NewFs()
	tfs.SetDiskLimit(limit)
	t.Cleanup(func() { _ = os.RemoveAll(tfs.tmpDir) })
	return tfs.Filesystem, tfs.Path()
}

func usageAfterScan(t *testing.T, fs *Filesystem) int64 {
	t.Helper()
	size, err := fs.DirectorySize(".")
	if err != nil {
		t.Fatal(err)
	}
	return size
}

// Deleting one name of a file that has other hard links frees nothing, so it
// must not lower the usage the server is held to.
func TestDeleteHardLinkedFileDoesNotLowerUsage(t *testing.T) {
	fs, root := newQuotaLinkTestFs(t, 1<<20)
	if err := fs.Write("file.bin", bytes.NewReader(make([]byte, 4096)), 4096, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "file.bin"), filepath.Join(root, "link.bin")); err != nil {
		t.Fatal(err)
	}
	before := fs.CachedUsage()

	if err := fs.Delete("link.bin"); err != nil {
		t.Fatal(err)
	}
	if got := fs.CachedUsage(); got != before {
		t.Fatalf("expected usage to stay %d after removing a hard link, got %d", before, got)
	}
	if got, want := fs.CachedUsage(), usageAfterScan(t, fs); got != want {
		t.Fatalf("expected usage %d to match a scan of %d", got, want)
	}
}

// A directory only frees the files whose hard links are all inside it.
func TestDeleteDirectoryCountsHardLinksOutsideIt(t *testing.T) {
	fs, root := newQuotaLinkTestFs(t, 1<<20)
	for _, f := range []string{"dir/inside.bin", "dir/shared.bin"} {
		if err := fs.Write(f, bytes.NewReader(make([]byte, 1000)), 1000, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	server := root
	// Both links of inside.bin are in the directory, shared.bin has one outside.
	if err := os.Link(filepath.Join(server, "dir", "inside.bin"), filepath.Join(server, "dir", "inside2.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(server, "dir", "shared.bin"), filepath.Join(server, "outside.bin")); err != nil {
		t.Fatal(err)
	}
	before := fs.CachedUsage()

	if err := fs.Delete("dir"); err != nil {
		t.Fatal(err)
	}
	if got, want := before-fs.CachedUsage(), int64(1000); got != want {
		t.Fatalf("expected deleting the directory to free %d bytes, freed %d", want, got)
	}
	if got, want := fs.CachedUsage(), usageAfterScan(t, fs); got != want {
		t.Fatalf("expected usage %d to match a scan of %d", got, want)
	}
}

// Overwriting a file accounts for the file that was actually replaced.
func TestWriteReplacesFileAccounting(t *testing.T) {
	fs, _ := newQuotaLinkTestFs(t, 1<<20)
	if err := fs.Write("file.txt", strings.NewReader(strings.Repeat("a", 500)), 500, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Write("file.txt", strings.NewReader(strings.Repeat("b", 200)), 200, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := fs.CachedUsage(), usageAfterScan(t, fs); got != want {
		t.Fatalf("expected usage %d to match a scan of %d", got, want)
	}
}

// A write that does not fit is refused without creating the file.
func TestWriteOverLimitDoesNotCreateFile(t *testing.T) {
	fs, root := newQuotaLinkTestFs(t, 100)
	err := fs.Write("big.bin", bytes.NewReader(make([]byte, 1000)), 1000, 0o644)
	if err == nil {
		t.Fatal("expected a write over the limit to be refused")
	}
	if _, err := os.Stat(filepath.Join(root, "big.bin")); !os.IsNotExist(err) {
		t.Fatalf("expected the refused file not to be created, got %v", err)
	}
	if got := fs.CachedUsage(); got != 0 {
		t.Fatalf("expected no usage after a refused write, got %d", got)
	}
}

// Truncating a file through Touch frees its space, even with other hard links,
// since the file is changed in place.
func TestTouchTruncateAccounting(t *testing.T) {
	fs, root := newQuotaLinkTestFs(t, 1<<20)
	if err := fs.Write("file.bin", bytes.NewReader(make([]byte, 2048)), 2048, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "file.bin"), filepath.Join(root, "link.bin")); err != nil {
		t.Fatal(err)
	}
	f, err := fs.Touch("file.bin", os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := fs.CachedUsage(), usageAfterScan(t, fs); got != want {
		t.Fatalf("expected usage %d to match a scan of %d", got, want)
	}
}

// Copies are created under a new name and accounted for.
func TestCopyCreatesNewFile(t *testing.T) {
	fs, root := newQuotaLinkTestFs(t, 1<<20)
	if err := fs.Write("file.txt", strings.NewReader("hello"), 5, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Copy("file.txt"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected the original and a copy, got %d entries", len(entries))
	}
	if got, want := fs.CachedUsage(), usageAfterScan(t, fs); got != want {
		t.Fatalf("expected usage %d to match a scan of %d", got, want)
	}
}

// A copy that does not fit is refused and leaves nothing behind.
func TestCopyOverLimitLeavesNothing(t *testing.T) {
	fs, root := newQuotaLinkTestFs(t, 1500)
	if err := fs.Write("file.bin", bytes.NewReader(make([]byte, 1000)), 1000, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Copy("file.bin"); err == nil {
		t.Fatal("expected a copy over the limit to be refused")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the original file, got %d entries", len(entries))
	}
}
