package sftp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/apex/log"
	pkgsftp "github.com/pkg/sftp"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/filesystem"
)

// newDenylistHandler returns a handler over a server directory holding
// plugins/secret.yml (denylisted), plugins/ok.txt and evil/secret.yml.
func newDenylistHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	cfg := &config.Configuration{AuthenticationToken: "test-token"}
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)
	initTestDatabase(t)

	srv, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, f := range []string{"plugins/secret.yml", "plugins/ok.txt", "evil/secret.yml"} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := filesystem.New(root, 0, []string{"/plugins/secret.yml"})
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{
		server:      srv,
		fs:          fs,
		events:      &eventHandler{},
		permissions: []string{"*"},
		logger:      log.WithField("test", t.Name()),
	}, root
}

func TestFilecmdRmdirKeepsDenylistedFiles(t *testing.T) {
	for _, method := range []string{"Rmdir", "Remove"} {
		t.Run(method, func(t *testing.T) {
			h, root := newDenylistHandler(t)
			err := h.Filecmd(pkgsftp.NewRequest(method, "/plugins"))
			if !errors.Is(err, pkgsftp.ErrSSHFxPermissionDenied) {
				t.Errorf("expected permission denied, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "plugins/secret.yml")); err != nil {
				t.Errorf("expected the denylisted file to be kept, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "plugins/ok.txt")); !os.IsNotExist(err) {
				t.Errorf("expected the other file to be deleted, got %v", err)
			}

			// A directory with nothing denylisted in it is still removed.
			if err := h.Filecmd(pkgsftp.NewRequest(method, "/evil")); !errors.Is(err, pkgsftp.ErrSSHFxOk) {
				t.Errorf("expected removing evil/ to succeed, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "evil")); !os.IsNotExist(err) {
				t.Errorf("expected evil/ to be deleted, got %v", err)
			}
		})
	}
}

func TestFilecmdRenameChecksDirectoryContents(t *testing.T) {
	h, root := newDenylistHandler(t)

	r := pkgsftp.NewRequest("Rename", "/plugins")
	r.Target = "/p2"
	if err := h.Filecmd(r); err == nil || errors.Is(err, pkgsftp.ErrSSHFxOk) {
		t.Errorf("expected renaming plugins/ to be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "plugins/secret.yml")); err != nil {
		t.Errorf("expected plugins/secret.yml to stay in place, got %v", err)
	}

	// Moving evil/ onto plugins/ would plant evil/secret.yml at a protected path.
	if err := os.RemoveAll(filepath.Join(root, "plugins")); err != nil {
		t.Fatal(err)
	}
	r = pkgsftp.NewRequest("Rename", "/evil")
	r.Target = "/plugins"
	if err := h.Filecmd(r); err == nil || errors.Is(err, pkgsftp.ErrSSHFxOk) {
		t.Errorf("expected renaming evil/ to plugins/ to be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "plugins")); !os.IsNotExist(err) {
		t.Errorf("expected plugins/ not to be created, got %v", err)
	}
}
