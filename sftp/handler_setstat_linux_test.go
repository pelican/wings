//go:build linux

package sftp

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/apex/log"
	pkgsftp "github.com/pkg/sftp"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/filesystem"
)

func TestSetstatAppliesTimesToDirectory(t *testing.T) {
	cfg := &config.Configuration{AuthenticationToken: "test-token"}
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)
	srv, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fs, err := filesystem.New(root, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := Handler{
		server:      srv,
		fs:          fs,
		permissions: []string{PermissionFileUpdate},
		logger:      log.WithField("test", t.Name()),
	}
	dir := filepath.Join(root, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	atime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	mtime := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	request := pkgsftp.NewRequest("Setstat", "/dir")
	request.Flags = 8 // SSH_FILEXFER_ATTR_ACMODTIME
	request.Attrs = make([]byte, 8)
	binary.BigEndian.PutUint32(request.Attrs[0:], uint32(atime.Unix()))
	binary.BigEndian.PutUint32(request.Attrs[4:], uint32(mtime.Unix()))

	if err := h.Filecmd(request); !errors.Is(err, pkgsftp.ErrSSHFxOk) {
		t.Fatalf("expected setstat to succeed, got %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(mtime) {
		t.Fatalf("expected mtime %v, got %v", mtime, st.ModTime())
	}
	sys := st.Sys().(*syscall.Stat_t)
	// Do not remove these "redundant" type-casts, they are required for 32-bit builds to work.
	if got := time.Unix(int64(sys.Atim.Sec), int64(sys.Atim.Nsec)); !got.Equal(atime) {
		t.Fatalf("expected atime %v, got %v", atime, got)
	}
}
