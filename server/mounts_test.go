package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsDevicePath(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skip("/dev/null is not available")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	devLink := filepath.Join(dir, "dev-link")
	if err := os.Symlink("/dev", devLink); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(dir, "file-link")
	if err := os.Symlink(file, fileLink); err != nil {
		t.Fatal(err)
	}

	cases := map[string]bool{
		"/dev/null":                   true,
		"/dev":                        true,
		devLink:                       true,
		dir:                           false,
		file:                          false,
		fileLink:                      false,
		filepath.Join(dir, "missing"): false,
	}
	for path, want := range cases {
		got, err := isDevicePath(path)
		if err != nil {
			t.Errorf("isDevicePath(%q) returned error: %v", path, err)
		} else if got != want {
			t.Errorf("isDevicePath(%q) = %v, want %v", path, got, want)
		}
	}
}
