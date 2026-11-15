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
	"time"
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
		// The install lock file stays by design; staging and backup copies must not.
		if strings.HasPrefix(e.Name(), ".") && e.Name() != installLockName {
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

type tarEntry struct {
	name, body, link string
	typ              byte
}

// writeTar builds a .tar.gz from explicit entries, including links.
func writeTar(t *testing.T, entries []tarEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typ, Linkname: e.link, Size: int64(len(e.body))}
		if e.typ == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []interface{ Close() error }{tw, gz, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestExtractArchiveKeepsInternalSymlinks(t *testing.T) {
	dest := t.TempDir()
	archive := writeTar(t, []tarEntry{
		{name: "main.py", typ: tar.TypeSymlink, link: "src/main.py"},
		{name: "src/main.py", typ: tar.TypeReg, body: "print('hi')\n"},
	})
	if err := extractArchive(context.Background(), archive, dest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dest, "main.py"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("main.py should be a symlink, got %v %v", info, err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "main.py"))
	if err != nil || string(b) != "print('hi')\n" {
		t.Fatalf("reading through the link: %q %v", b, err)
	}
}

func TestExtractArchiveRefusesLinksLeavingThePlugin(t *testing.T) {
	cases := map[string][]tarEntry{
		"absolute target": {{name: "run", typ: tar.TypeSymlink, link: "/bin/sh"}},
		"relative escape": {{name: "run", typ: tar.TypeSymlink, link: "../../outside"}},
		// Each link looks fine on its own; together they lead to dest's parent.
		"chain escape": {
			{name: "root", typ: tar.TypeSymlink, link: "."},
			{name: "x/up", typ: tar.TypeSymlink, link: "../root/.."},
			{name: "x/keep", typ: tar.TypeReg, body: "k"},
		},
		"hard link": {
			{name: "a", typ: tar.TypeReg, body: "a"},
			{name: "b", typ: tar.TypeLink, link: "a"},
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			if err := extractArchive(context.Background(), writeTar(t, entries), t.TempDir()); err == nil {
				t.Fatal("expected the archive to be refused")
			}
		})
	}
}

func TestExtractArchiveNeverWritesThroughALink(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "plugin")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := writeTar(t, []tarEntry{
		{name: "out", typ: tar.TypeSymlink, link: ".."},
		{name: "out/escaped.txt", typ: tar.TypeReg, body: "x"},
	})
	_ = extractArchive(context.Background(), archive, dest)
	if _, err := os.Stat(filepath.Join(parent, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("a file was written outside the plugin directory (stat err = %v)", err)
	}
}

func TestInstallsOnTheSameDirectoryWaitForEachOther(t *testing.T) {
	dir := t.TempDir()
	pluginDir(t, filepath.Join(dir, "audit"), "audit v1")
	pluginDir(t, filepath.Join(dir, ".wings-install-a"), "audit from A")
	pluginDir(t, filepath.Join(dir, ".wings-install-b"), "audit from B")
	dest := filepath.Join(dir, "audit")
	a := &StagedInstall{DestDir: dest, StagingDir: filepath.Join(dir, ".wings-install-a")}
	b := &StagedInstall{DestDir: dest, StagingDir: filepath.Join(dir, ".wings-install-b")}

	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	bDone := make(chan error, 1)
	go func() {
		err := b.Commit()
		if err == nil {
			err = b.Finish()
		}
		bDone <- err
	}()
	select {
	case err := <-bDone:
		t.Fatalf("B committed while A still held the install (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	// A's approval failed, so A rolls back; then B goes ahead.
	if err := a.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B never got the install lock")
	}
	if got := readID(t, dest); got != "audit from B" {
		t.Fatalf("audit = %q, want B's version", got)
	}
}

func TestExtractArchiveRefusesEntriesOutsideTheDestination(t *testing.T) {
	for _, name := range []string{"../escaped.txt", "a/../../escaped.txt", "/escaped.txt"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "plugin")
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			archive := writeTar(t, []tarEntry{{name: name, typ: tar.TypeReg, body: "x"}})
			if err := extractArchive(context.Background(), archive, dest); err == nil {
				t.Fatal("expected the entry to be refused")
			}
			if _, err := os.Stat(filepath.Join(parent, "escaped.txt")); !os.IsNotExist(err) {
				t.Fatalf("a file was written outside the destination (stat err = %v)", err)
			}
		})
	}
}

func TestExtractArchiveCountsImpliedDirectories(t *testing.T) {
	prev := maxUpdateExtractEntries
	maxUpdateExtractEntries = 5
	t.Cleanup(func() { maxUpdateExtractEntries = prev })

	// One entry, but its path implies seven directories.
	archive := writeTar(t, []tarEntry{{name: "a/b/c/d/e/f/g/file", typ: tar.TypeReg, body: "x"}})
	err := extractArchive(context.Background(), archive, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "more than 5 files and directories") {
		t.Fatalf("directories implied by an entry's path must count against the limit, got %v", err)
	}
	if err := extractArchive(context.Background(), writeTar(t, []tarEntry{{name: "a/b/file", typ: tar.TypeReg, body: "x"}}), t.TempDir()); err != nil {
		t.Fatalf("three objects should fit under a limit of five: %v", err)
	}
}

func TestExtractArchiveRefusesARegularFileNamedDot(t *testing.T) {
	archive := writeTar(t, []tarEntry{{name: ".", typ: tar.TypeReg, body: strings.Repeat("x", 4096)}})
	if err := extractArchive(context.Background(), archive, t.TempDir()); err == nil {
		t.Fatal("a regular file named \".\" must be refused, not skipped unread")
	}
	if err := extractArchive(context.Background(), writeTar(t, []tarEntry{{name: "./", typ: tar.TypeDir}, {name: "./plugin.yaml", typ: tar.TypeReg, body: "x"}}), t.TempDir()); err != nil {
		t.Fatalf("a \"./\" root directory entry is normal and must still work: %v", err)
	}
}

func TestUpdateLeavesThePluginDataDirectoryAlone(t *testing.T) {
	dir := t.TempDir()
	pluginDir(t, filepath.Join(dir, "foo"), "foo v1")
	pluginDir(t, filepath.Join(dir, DataDirName, "foo"), "operator settings")
	pluginDir(t, filepath.Join(dir, ".wings-install-1"), "foo v2")

	stage := &StagedInstall{DestDir: filepath.Join(dir, "foo"), StagingDir: filepath.Join(dir, ".wings-install-1")}
	if err := stage.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := stage.Finish(); err != nil {
		t.Fatal(err)
	}
	if got := readID(t, filepath.Join(dir, DataDirName, "foo")); got != "operator settings" {
		t.Fatalf("the update touched the data directory: %q", got)
	}
}
