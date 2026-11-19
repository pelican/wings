package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/server/filesystem"
)

func newIgnoreTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	config.Set(&config.Configuration{
		AuthenticationToken: "abc",
		System:              config.SystemConfiguration{RootDirectory: "/server"},
	})

	root := t.TempDir()
	fs, err := filesystem.New(root, 0, []string{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return &Server{fs: fs}, root
}

func TestGetServerwideIgnoredFiles(t *testing.T) {
	t.Run("no ignore file", func(t *testing.T) {
		s, _ := newIgnoreTestServer(t)
		got, err := s.getServerwideIgnoredFiles()
		if err != nil || got != "" {
			t.Fatalf("expected no ignored files, got %q, %v", got, err)
		}
	})

	t.Run("falls back to .pteroignore", func(t *testing.T) {
		s, root := newIgnoreTestServer(t)
		if err := os.WriteFile(filepath.Join(root, ".pteroignore"), []byte("logs/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := s.getServerwideIgnoredFiles()
		if err != nil || got != "logs/\n" {
			t.Fatalf("expected .pteroignore contents, got %q, %v", got, err)
		}
	})

	t.Run(".pelicanignore takes precedence", func(t *testing.T) {
		s, root := newIgnoreTestServer(t)
		if err := os.WriteFile(filepath.Join(root, ".pteroignore"), []byte("logs/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".pelicanignore"), []byte("cache/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := s.getServerwideIgnoredFiles()
		if err != nil || got != "cache/\n" {
			t.Fatalf("expected .pelicanignore contents, got %q, %v", got, err)
		}
	})
}
