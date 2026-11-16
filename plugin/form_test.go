package plugin

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetectForm(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		form   Form
		binary []string
	}{
		{"text only", map[string]string{"plugin.yaml": "name: x\n", "main.py": "print('héllo ✓')\n", "empty": ""}, FormSource, nil},
		{"a NUL byte", map[string]string{"plugin.yaml": "name: x\n", "data/blob": "abc\x00def"}, FormBinary, []string{"data/blob"}},
		{"invalid UTF-8", map[string]string{"plugin.yaml": "name: x\n", "latin1.txt": "caf\xe9\n"}, FormBinary, []string{"latin1.txt"}},
		{"a cut-off rune at the end", map[string]string{"plugin.yaml": "name: x\n", "cut": "ok \xe2\x9c"}, FormBinary, []string{"cut"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DetectForm(writeFiles(t, c.files))
			if err != nil {
				t.Fatal(err)
			}
			if got.Form != c.form || !slices.Equal(got.BinaryFiles, c.binary) {
				t.Fatalf("got %+v, want form %s with binary files %v", got, c.form, c.binary)
			}
			if got.BinaryFiles == nil {
				t.Fatal("BinaryFiles must be an empty list, not nil, so it encodes as []")
			}
		})
	}
}

// TestDetectFormFindsAGoBinary uses the test binary itself: a real Go
// executable.
func TestDetectFormFindsAGoBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := writeFiles(t, map[string]string{"plugin.yaml": "name: x\n", "main.go": "package main\n"})
	in, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(filepath.Join(dir, "plugin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()

	got, err := DetectForm(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Form != FormBinary || !slices.Equal(got.BinaryFiles, []string{"plugin"}) {
		t.Fatalf("got %+v, want the compiled binary listed", got)
	}
}

func TestDetectFormListsAtMost20BinaryFiles(t *testing.T) {
	files := map[string]string{"plugin.yaml": "name: x\n"}
	for i := range 25 {
		files[fmt.Sprintf("lib/%02d.so", i)] = "\x7fELF\x00"
	}
	got, err := DetectForm(writeFiles(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if got.Form != FormBinary || len(got.BinaryFiles) != MaxBinaryFiles || got.BinaryCount != 25 {
		t.Fatalf("got %d listed of %d, want %d of 25", len(got.BinaryFiles), got.BinaryCount, MaxBinaryFiles)
	}
	if got.BinaryFiles[0] != "lib/00.so" || got.BinaryFiles[19] != "lib/19.so" {
		t.Fatalf("binary files should be listed in path order: %v", got.BinaryFiles)
	}
}

// TestIsTextAcrossReads covers a multi-byte rune split between two reads.
func TestIsTextAcrossReads(t *testing.T) {
	text := strings.Repeat("ab✓€𝄞", 1000)
	if ok, err := isText(iotest.OneByteReader(strings.NewReader(text))); err != nil || !ok {
		t.Fatalf("valid UTF-8 read one byte at a time: %v, %v", ok, err)
	}
	long := strings.Repeat("x", 64<<10-1) + "✓" + strings.Repeat("y", 10)
	if ok, err := isText(strings.NewReader(long)); err != nil || !ok {
		t.Fatalf("a rune across the buffer boundary: %v, %v", ok, err)
	}
	if ok, _ := isText(iotest.OneByteReader(strings.NewReader("ab\x00"))); ok {
		t.Fatal("a NUL byte is not text")
	}
}

func TestCachedFormFollowsChanges(t *testing.T) {
	prev := formRacyWindow
	formRacyWindow = 50 * time.Millisecond
	t.Cleanup(func() { formRacyWindow = prev })
	dir := writeFiles(t, map[string]string{"plugin.yaml": "name: x\n", "main.py": "print(1)\n"})

	// Just written: not remembered yet, since a change in the same clock
	// tick wouldn't show in the files' times.
	before := formDetections.Load()
	for range 2 {
		if _, err := CachedForm(dir); err != nil {
			t.Fatal(err)
		}
	}
	if n := formDetections.Load() - before; n != 2 {
		t.Fatalf("a directory changed moments ago was read %d times in 2 calls, want 2", n)
	}

	time.Sleep(2 * formRacyWindow)
	before = formDetections.Load()
	for range 3 {
		got, err := CachedForm(dir)
		if err != nil || got.Form != FormSource {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
	if n := formDetections.Load() - before; n != 1 {
		t.Fatalf("files were read %d times for an unchanged directory, want once", n)
	}
	// Same size, same inode: only the times change.
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := CachedForm(dir)
	if err != nil || got.Form != FormBinary {
		t.Fatalf("after a change: %+v, %v; want binary", got, err)
	}
}
