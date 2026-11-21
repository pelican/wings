package filesystem

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func zipWithEntry(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Entries with names longer than any path Linux accepts are refused, both when
// checking the space an archive needs and when extracting it.
func TestDecompressRefusesOverlongEntryNames(t *testing.T) {
	for _, limit := range []int64{0, 1 << 20} {
		tfs := NewFs()
		t.Cleanup(func() { _ = os.RemoveAll(tfs.tmpDir) })
		tfs.SetDiskLimit(limit)

		long := strings.Repeat("a/", maxArchiveEntryNameLength/2) + "file.txt"
		b := zipWithEntry(t, long)
		if err := tfs.Write("long.zip", bytes.NewReader(b), int64(len(b)), 0o644); err != nil {
			t.Fatal(err)
		}

		if limit > 0 {
			if err := tfs.SpaceAvailableForDecompression(context.Background(), "/", "long.zip"); err == nil {
				t.Errorf("limit %d: expected the space check to refuse the archive", limit)
			}
		}
		if err := tfs.DecompressFile(context.Background(), "/", "long.zip"); err == nil {
			t.Errorf("limit %d: expected extraction to refuse the archive", limit)
		}

		// A name within the limit is still extracted.
		ok := zipWithEntry(t, "dir/file.txt")
		if err := tfs.Write("ok.zip", bytes.NewReader(ok), int64(len(ok)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := tfs.DecompressFile(context.Background(), "/", "ok.zip"); err != nil {
			t.Errorf("limit %d: expected a normal archive to be extracted: %v", limit, err)
		}
	}
}
