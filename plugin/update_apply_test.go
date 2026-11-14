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

// pluginDir writes a directory holding one file whose contents identify it.
func pluginDir(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "id"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readID(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(path, "id"))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func hiddenEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestUpdateLeavesAPluginNamedBakAlone(t *testing.T) {
	dir := t.TempDir()
	pluginDir(t, filepath.Join(dir, "audit"), "audit v1")
	pluginDir(t, filepath.Join(dir, "audit.bak"), "a different plugin")
	pluginDir(t, filepath.Join(dir, ".wings-install-1"), "audit v2")

	stage := &StagedInstall{DestDir: filepath.Join(dir, "audit"), StagingDir: filepath.Join(dir, ".wings-install-1")}
	if err := stage.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := stage.Finish(); err != nil {
		t.Fatal(err)
	}

	if got := readID(t, filepath.Join(dir, "audit.bak")); got != "a different plugin" {
		t.Fatalf("audit.bak was touched: %q", got)
	}
	if got := readID(t, filepath.Join(dir, "audit")); got != "audit v2" {
		t.Fatalf("audit = %q, want v2", got)
	}
	if h := hiddenEntries(t, dir); len(h) != 0 {
		t.Fatalf("left behind: %v", h)
	}
}

func TestRollbackRestoresThePreviousVersion(t *testing.T) {
	dir := t.TempDir()
	pluginDir(t, filepath.Join(dir, "audit"), "audit v1")
	pluginDir(t, filepath.Join(dir, ".wings-install-1"), "audit v2")

	stage := &StagedInstall{DestDir: filepath.Join(dir, "audit"), StagingDir: filepath.Join(dir, ".wings-install-1")}
	if err := stage.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := readID(t, filepath.Join(dir, "audit")); got != "audit v2" {
		t.Fatalf("after commit audit = %q", got)
	}
	// Recording the approval failed: the update has to be undone.
	if err := stage.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := readID(t, filepath.Join(dir, "audit")); got != "audit v1" {
		t.Fatalf("after rollback audit = %q, want v1", got)
	}
	if h := hiddenEntries(t, dir); len(h) != 0 {
		t.Fatalf("left behind: %v", h)
	}
}

func TestRollbackOfAFreshInstallRemovesIt(t *testing.T) {
	dir := t.TempDir()
	pluginDir(t, filepath.Join(dir, ".wings-install-1"), "audit v1")

	stage := &StagedInstall{DestDir: filepath.Join(dir, "audit"), StagingDir: filepath.Join(dir, ".wings-install-1")}
	if err := stage.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := stage.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit")); !os.IsNotExist(err) {
		t.Fatalf("a rolled-back fresh install should be gone, stat err = %v", err)
	}
}
