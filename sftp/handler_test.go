package sftp

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apex/log"
	pkgsftp "github.com/pkg/sftp"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/filesystem"
)

type writeAtFunc func([]byte, int64) (int, error)

func (f writeAtFunc) WriteAt(p []byte, off int64) (int, error) {
	return f(p, off)
}

func TestHandlerDeniesAccessWhenServerIsInProtectedState(t *testing.T) {
	tests := []struct {
		name string
		set  func(*server.Server)
	}{
		{
			name: "installing",
			set: func(s *server.Server) {
				s.SetInstalling(true)
			},
		},
		{
			name: "transferring",
			set: func(s *server.Server) {
				s.SetTransferring(true)
			},
		},
		{
			name: "restoring",
			set: func(s *server.Server) {
				s.SetRestoring(true)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, err := server.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			tt.set(srv)

			h := Handler{
				server:      srv,
				permissions: []string{"*"},
			}

			if h.can(PermissionFileCreate) {
				t.Fatal("expected SFTP access to be denied")
			}
		})
	}
}

func TestWriterDeniesWritesWhenServerEntersProtectedState(t *testing.T) {
	tests := []struct {
		name string
		set  func(*server.Server)
	}{
		{
			name: "installing",
			set: func(s *server.Server) {
				s.SetInstalling(true)
			},
		},
		{
			name: "transferring",
			set: func(s *server.Server) {
				s.SetTransferring(true)
			},
		},
		{
			name: "restoring",
			set: func(s *server.Server) {
				s.SetRestoring(true)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, err := server.New(nil)
			if err != nil {
				t.Fatal(err)
			}

			var called bool
			writer := quotaWriterAt{
				server: srv,
				WriterAt: writeAtFunc(func(_ []byte, _ int64) (int, error) {
					called = true
					return 1, nil
				}),
			}
			tt.set(srv)

			n, err := writer.WriteAt([]byte("x"), 0)
			if !errors.Is(err, pkgsftp.ErrSSHFxPermissionDenied) {
				t.Fatalf("expected permission denied, got %v", err)
			}
			if n != 0 {
				t.Fatalf("expected zero bytes written, got %d", n)
			}
			if called {
				t.Fatal("expected underlying writer not to be called")
			}
		})
	}
}

func TestWriterForwardsWritesWhenServerIsAvailable(t *testing.T) {
	srv, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	writer := quotaWriterAt{
		server: srv,
		WriterAt: writeAtFunc(func(p []byte, _ int64) (int, error) {
			return len(p), io.EOF
		}),
	}

	n, err := writer.WriteAt([]byte("test"), 0)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected forwarded error, got %v", err)
	}
	if n != 4 {
		t.Fatalf("expected forwarded byte count, got %d", n)
	}
}

func TestHandlerRejectsMalformedSetstatAttributes(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "test-token"})
	srv, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := filesystem.New(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := Handler{
		server:      srv,
		fs:          fs,
		permissions: []string{PermissionFileUpdate},
		logger:      log.WithField("test", t.Name()),
	}
	request := pkgsftp.NewRequest("Setstat", "/")
	request.Flags = 1 // SSH_FILEXFER_ATTR_SIZE

	if err := h.Filecmd(request); !errors.Is(err, pkgsftp.ErrSSHFxBadMessage) {
		t.Fatalf("expected bad message, got %v", err)
	}

	request.Flags = sftpAttributeExtended
	request.Attrs = make([]byte, 4)
	binary.BigEndian.PutUint32(request.Attrs, ^uint32(0))
	if err := h.Filecmd(request); !errors.Is(err, pkgsftp.ErrSSHFxBadMessage) {
		t.Fatalf("expected extended attributes to be rejected, got %v", err)
	}
}

func TestSetstatMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     uint32
		expected uint32
	}{
		{name: "file permissions", mode: 0o600, expected: 0o600},
		{name: "default permissions", mode: 0o000, expected: 0o644},
		{name: "directory permissions", mode: 0o040700, expected: 0o755},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := pkgsftp.NewRequest("Setstat", "/test")
			request.Flags = 4 // SSH_FILEXFER_ATTR_PERMISSIONS
			request.Attrs = make([]byte, 4)
			binary.BigEndian.PutUint32(request.Attrs, tt.mode)

			mode, err := setstatMode(request)
			if err != nil {
				t.Fatal(err)
			}
			if uint32(mode) != tt.expected {
				t.Fatalf("expected mode %04o, got %04o", tt.expected, mode)
			}
		})
	}
}

func TestSetstatAppliesTimes(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "test-token"})
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
	p := filepath.Join(root, "test.txt")
	if err := os.WriteFile(p, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}

	atime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	mtime := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	request := pkgsftp.NewRequest("Setstat", "/test.txt")
	request.Flags = 8 // SSH_FILEXFER_ATTR_ACMODTIME
	request.Attrs = make([]byte, 8)
	binary.BigEndian.PutUint32(request.Attrs[0:], uint32(atime.Unix()))
	binary.BigEndian.PutUint32(request.Attrs[4:], uint32(mtime.Unix()))

	if err := h.Filecmd(request); !errors.Is(err, pkgsftp.ErrSSHFxOk) {
		t.Fatalf("expected setstat to succeed, got %v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(mtime) {
		t.Fatalf("expected mtime %v, got %v", mtime, st.ModTime())
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("expected mode 0755 to be kept, got %04o", st.Mode().Perm())
	}

	request.Flags = 4 | 8 // SSH_FILEXFER_ATTR_PERMISSIONS | SSH_FILEXFER_ATTR_ACMODTIME
	request.Attrs = make([]byte, 12)
	binary.BigEndian.PutUint32(request.Attrs[0:], 0o600)
	binary.BigEndian.PutUint32(request.Attrs[4:], uint32(atime.Unix()))
	binary.BigEndian.PutUint32(request.Attrs[8:], uint32(atime.Unix()))

	if err := h.Filecmd(request); !errors.Is(err, pkgsftp.ErrSSHFxOk) {
		t.Fatalf("expected setstat to succeed, got %v", err)
	}
	st, err = os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(atime) {
		t.Fatalf("expected mtime %v, got %v", atime, st.ModTime())
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %04o", st.Mode().Perm())
	}
}
