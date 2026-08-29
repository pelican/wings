package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/server/filesystem"
)

func TestBackupGenerateRequiresUuidIdentifier(t *testing.T) {
	tests := map[string]func(string) BackupInterface{
		"local": func(identifier string) BackupInterface {
			return NewLocal(nil, identifier, "ce6ee345-6729-4aed-8fed-c866c535a69d", "")
		},
		"s3": func(identifier string) BackupInterface {
			return NewS3(nil, identifier, "ce6ee345-6729-4aed-8fed-c866c535a69d", "")
		},
	}

	for name, createBackup := range tests {
		t.Run(name, func(t *testing.T) {
			testBackupGenerateRequiresUuidIdentifier(t, createBackup)
		})
	}
}

func TestBackupPathUsesBackupDirectory(t *testing.T) {
	backupDir := t.TempDir()
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System: config.SystemConfiguration{
			BackupDirectory: backupDir,
		},
	})

	for _, identifier := range []string{
		"11111111-1111-1111-1111-111111111111",
		"../target/archive",
		"nested/archive",
	} {
		b := NewLocal(nil, identifier, "ce6ee345-6729-4aed-8fed-c866c535a69d", "")
		rel, err := filepath.Rel(backupDir, b.Path())
		if err != nil {
			t.Fatal(err)
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			t.Fatalf("expected backup path %q to remain under %q", b.Path(), backupDir)
		}
	}
}

func TestBackupRestoreDoesNotSkipSymlinks(t *testing.T) {
	backupDir := t.TempDir()
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System: config.SystemConfiguration{
			BackupDirectory: backupDir,
		},
	})

	archiveData := buildTestArchive(t,
		map[string]string{"real_file.txt": "hello, world!\n"},
		map[string]string{"link_to_file.txt": "real_file.txt"},
	)

	t.Run("local", func(t *testing.T) {
		b := NewLocal(nil, "11111111-1111-1111-1111-111111111111", "ce6ee345-6729-4aed-8fed-c866c535a69d", "")
		if err := os.MkdirAll(filepath.Dir(b.Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b.Path(), archiveData, 0o600); err != nil {
			t.Fatal(err)
		}
		assertRestoreHandlesSymlinks(t, b.Restore, nil)
	})

	t.Run("s3", func(t *testing.T) {
		b := NewS3(nil, "22222222-2222-2222-2222-222222222222", "ce6ee345-6729-4aed-8fed-c866c535a69d", "")
		assertRestoreHandlesSymlinks(t, b.Restore, bytes.NewReader(archiveData))
	})
}

func assertRestoreHandlesSymlinks(t *testing.T, restore func(context.Context, io.Reader, RestoreCallback) error, reader io.Reader) {
	t.Helper()

	type restoredEntry struct {
		linkTarget string
		hasReader  bool
	}
	got := map[string]restoredEntry{}

	err := restore(context.Background(), reader, func(file string, info fs.FileInfo, linkTarget string, r io.ReadCloser) error {
		got[file] = restoredEntry{linkTarget: linkTarget, hasReader: r != nil}
		if r != nil {
			_ = r.Close()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	f, ok := got["real_file.txt"]
	if !ok {
		t.Fatal("expected callback to be invoked for the regular file")
	}
	if !f.hasReader {
		t.Error("expected a reader for the regular file entry")
	}
	if f.linkTarget != "" {
		t.Errorf("expected no link target for the regular file entry, got %q", f.linkTarget)
	}

	link, ok := got["link_to_file.txt"]
	if !ok {
		t.Fatal("expected callback to be invoked for the symlink instead of silently skipping it")
	}
	if link.hasReader {
		t.Error("expected no reader for the symlink entry")
	}
	if link.linkTarget != "real_file.txt" {
		t.Errorf("expected link target %q, got %q", "real_file.txt", link.linkTarget)
	}
}

func buildTestArchive(t *testing.T, files map[string]string, symlinks map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, contents := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(contents)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}

	for name, target := range symlinks {
		hdr := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeSymlink,
			Linkname: target,
			Mode:     0o777,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testBackupGenerateRequiresUuidIdentifier(t *testing.T, createBackup func(string) BackupInterface) {
	t.Helper()

	root := t.TempDir()
	backupDir := filepath.Join(root, "backups")
	targetDir := filepath.Join(root, "target")
	serverDir := filepath.Join(root, "server")
	for _, dir := range []string{backupDir, targetDir, serverDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System: config.SystemConfiguration{
			BackupDirectory: backupDir,
		},
	})

	if err := os.WriteFile(filepath.Join(serverDir, "file.txt"), []byte("server data"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys, err := filesystem.New(serverDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	existingArchive := filepath.Join(targetDir, "archive.tar.gz")
	existingArchiveContents := []byte("existing archive")
	if err := os.WriteFile(existingArchive, existingArchiveContents, 0o600); err != nil {
		t.Fatal(err)
	}

	b := createBackup("../target/archive")
	if _, err := b.Generate(context.Background(), fsys, ""); err == nil {
		t.Fatal("expected invalid backup identifier to be rejected")
	}

	got, err := os.ReadFile(existingArchive)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, existingArchiveContents) {
		return
	}
	t.Fatal("expected backup generation not to overwrite existing archive")
}
