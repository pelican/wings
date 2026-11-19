package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/server/backup"
	"github.com/pelican/wings/server/filesystem"
)

// suspendRecordingClient records whether the server was still suspended or marked
// as restoring when the Panel was told the restore finished.
type suspendRecordingClient struct {
	restoreTestClient
	server            **Server
	suspendedAtNotify *bool
	restoringAtNotify *bool
	notified          *bool
}

func (c suspendRecordingClient) SendRestorationStatus(context.Context, string, bool) error {
	*c.notified = true
	*c.suspendedAtNotify = (*c.server).IsSuspended()
	*c.restoringAtNotify = (*c.server).IsRestoring()
	return nil
}

// The Panel can start the server as soon as it hears the restore is done, which
// Wings refuses while the server is suspended or restoring, so both are cleared
// first.
func TestRestoreBackupUnsuspendsBeforeNotifyingPanel(t *testing.T) {
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

	if err := os.WriteFile(filepath.Join(serverDir, "server.properties"), []byte("motd=hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var s *Server
	var suspendedAtNotify, restoringAtNotify, notified bool
	client := suspendRecordingClient{server: &s, suspendedAtNotify: &suspendedAtNotify, restoringAtNotify: &restoringAtNotify, notified: &notified}

	var err error
	s, err = New(client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)
	s.cfg.Uuid = "0b9f4c1e-2d4f-4a8e-9c1b-3e5f7a9b1c2d"
	env := &terminateTestEnvironment{}
	env.state.Store(environment.ProcessOfflineState)
	s.Environment = env
	s.fs, err = filesystem.New(serverDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.fs.Close() })

	b := backup.NewLocal(client, "22222222-2222-4222-8222-222222222222", s.ID(), "")
	if _, err := b.Generate(context.Background(), s.Filesystem(), ""); err != nil {
		t.Fatal(err)
	}

	// The restore request marks the server as restoring before it restores.
	s.SetRestoring(true)
	if err := s.RestoreBackup(b, nil); err != nil {
		t.Fatal(err)
	}

	if !notified {
		t.Fatal("expected the Panel to be notified of the restore")
	}
	if suspendedAtNotify {
		t.Fatal("expected the server to be unsuspended before the Panel was notified")
	}
	if restoringAtNotify {
		t.Fatal("expected the server to no longer be restoring when the Panel was notified")
	}
	if s.IsSuspended() || s.IsRestoring() {
		t.Fatal("expected the server to be unsuspended and not restoring after the restore")
	}
}
