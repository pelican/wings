package plugin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyLimitedRejectsOversizedInput(t *testing.T) {
	var dst bytes.Buffer
	_, err := copyLimited(&dst, strings.NewReader("abcd"), 3)
	if err == nil {
		t.Fatal("expected oversized file to be rejected")
	}
}

func TestCopyLimitedAllowsMaxSizedInput(t *testing.T) {
	var dst bytes.Buffer
	if _, err := copyLimited(&dst, strings.NewReader("abc"), 3); err != nil {
		t.Fatalf("copyLimited: %v", err)
	}
	if got := dst.String(); got != "abc" {
		t.Fatalf("copied %q, want abc", got)
	}
}

// writeTarGz builds a .tar.gz with one regular file per entry of size bytes.
func writeTarGz(t *testing.T, files int, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := bytes.Repeat([]byte("x"), size)
	for i := 0; i < files; i++ {
		hdr := &tar.Header{Name: "f" + string(rune('a'+i)), Mode: 0o644, Size: int64(size), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractArchiveEnforcesTotalSize(t *testing.T) {
	prev := maxUpdateExtractBytes
	maxUpdateExtractBytes = 2048
	t.Cleanup(func() { maxUpdateExtractBytes = prev })

	// Three 1 KiB files: each is far under the per-file cap, 3 KiB in total.
	archive := writeTarGz(t, 3, 1024)
	err := extractArchive(context.Background(), archive, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "expands to more than") {
		t.Fatalf("expected the archive-wide size limit to stop extraction, got %v", err)
	}
	if err := extractArchive(context.Background(), writeTarGz(t, 2, 1024), t.TempDir()); err != nil {
		t.Fatalf("an archive exactly at the limit should extract: %v", err)
	}
}

func TestExtractArchiveEnforcesEntryCount(t *testing.T) {
	prev := maxUpdateExtractEntries
	maxUpdateExtractEntries = 2
	t.Cleanup(func() { maxUpdateExtractEntries = prev })

	err := extractArchive(context.Background(), writeTarGz(t, 3, 1), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "more than 2 entries") {
		t.Fatalf("expected the entry limit to stop extraction, got %v", err)
	}
}
