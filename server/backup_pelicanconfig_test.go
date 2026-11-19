package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/server/backup"
	"github.com/pelican/wings/server/filesystem"
)

// newPelicanConfigTestServer returns a server with its data in a temporary
// directory, for generating and restoring backups.
func newPelicanConfigTestServer(t *testing.T) (*Server, string) {
	t.Helper()

	serverDir := t.TempDir()
	cfg := &config.Configuration{
		AuthenticationToken: "abc",
		System: config.SystemConfiguration{
			RootDirectory:     filepath.Dir(serverDir),
			BackupDirectory:   t.TempDir(),
			DiskCheckInterval: 150,
		},
	}
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)

	s, err := New(restoreTestClient{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)
	s.cfg.Uuid = "ce6ee345-6729-4aed-8fed-c866c535a69d"
	env := &terminateTestEnvironment{}
	env.state.Store(environment.ProcessOfflineState)
	s.Environment = env
	s.fs, err = filesystem.New(serverDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.fs.Close() })

	return s, serverDir
}

// The server configuration is the first entry of the archive, where the Panel
// reads it from the start of the download, and is not restored into the
// server's files. A .pelicanconfig the server had itself is kept.
func TestBackupStoresPelicanConfigAsFirstEntry(t *testing.T) {
	s, serverDir := newPelicanConfigTestServer(t)

	if err := os.WriteFile(filepath.Join(serverDir, "server.properties"), []byte("motd=hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sorts before every other file, so it would be first without the prelude.
	if err := os.WriteFile(filepath.Join(serverDir, ".pelicanconfig"), []byte("the server's own file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pelicanConfig := []byte(`{"meta":{"version":"PLCN_SRV_v1"}}`)
	b := backup.NewLocal(restoreTestClient{}, "11111111-1111-4111-8111-111111111111", s.ID(), "")
	b.SetPelicanConfig(pelicanConfig)
	if _, err := b.Generate(context.Background(), s.Filesystem(), ""); err != nil {
		t.Fatal(err)
	}

	// Read the archive the way the Panel does: the first tar header is at the
	// very start of the decompressed stream.
	f, err := os.Open(b.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 512+len(pelicanConfig))
	if _, err := io.ReadFull(gz, raw); err != nil {
		t.Fatal(err)
	}
	if name := string(raw[:len(".pelicanconfig")]); name != ".pelicanconfig" || raw[len(name)] != 0 {
		t.Fatalf("expected the first header to be .pelicanconfig, got %q", raw[:100])
	}
	if typ := raw[156]; typ != tar.TypeReg {
		t.Fatalf("expected a regular file, got type %q", typ)
	}
	if got := string(raw[512:]); got != string(pelicanConfig) {
		t.Fatalf("expected the configuration right after the header, got %q", got)
	}

	// The server's own .pelicanconfig is in the archive after it.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := gz.Reset(f); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if len(names) != 3 || names[0] != ".pelicanconfig" || names[1] != ".pelicanconfig" || names[2] != "server.properties" {
		t.Fatalf("unexpected archive entries %v", names)
	}

	if err := s.Filesystem().TruncateRootDirectory(); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreBackup(b, nil); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(serverDir, ".pelicanconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the server's own file\n" {
		t.Fatalf("expected the server's own .pelicanconfig to be restored, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(serverDir, "server.properties")); err != nil {
		t.Fatalf("expected server.properties to be restored: %v", err)
	}
}

// A file on the disk is never mistaken for the server configuration, even when
// its header has the same name and is owned by a user named "pelican", which is
// the default Wings user.
func TestPelicanConfigEntryRequiresReservedOwner(t *testing.T) {
	onDisk := &tar.Header{Name: ".pelicanconfig", Typeflag: tar.TypeReg, Uname: "pelican", Gname: "pelican"}
	if filesystem.IsPreludeHeader(onDisk, backup.PelicanConfigFile) {
		t.Fatal("expected a .pelicanconfig owned by pelican:pelican not to be a prelude")
	}

	prelude := &tar.Header{Name: ".pelicanconfig", Typeflag: tar.TypeReg, Uname: filesystem.PreludeOwner, Gname: filesystem.PreludeOwner}
	if !filesystem.IsPreludeHeader(prelude, backup.PelicanConfigFile) {
		t.Fatal("expected the prelude header to be detected")
	}
}

// Without a configuration nothing is added to the archive, and restoring it does
// not create a .pelicanconfig.
func TestBackupWithoutPelicanConfig(t *testing.T) {
	s, serverDir := newPelicanConfigTestServer(t)

	if err := os.WriteFile(filepath.Join(serverDir, "server.properties"), []byte("motd=hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := backup.NewLocal(restoreTestClient{}, "22222222-2222-4222-8222-222222222222", s.ID(), "")
	if _, err := b.Generate(context.Background(), s.Filesystem(), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Filesystem().TruncateRootDirectory(); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreBackup(b, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(serverDir, ".pelicanconfig")); !os.IsNotExist(err) {
		t.Fatalf("expected no .pelicanconfig to be restored, got %v", err)
	}
}
