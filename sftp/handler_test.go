package sftp

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/apex/log"
	pkgsftp "github.com/pkg/sftp"

	"github.com/pelican/wings/server"
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
	srv, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := Handler{
		server:      srv,
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