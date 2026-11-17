package cmd

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/server"
)

// On a signal the server states are saved and the HTTP server stops, so that
// Wings exits instead of being killed.
func TestShutdownOnSignalSavesStatesAndStopsServer(t *testing.T) {
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.System.RootDirectory = t.TempDir()
	config.Set(cfg)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{ReadHeaderTimeout: time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()

	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	go shutdownOnSignal(signals, server.NewEmptyManager(nil), srv, done)
	signals <- syscall.SIGTERM

	select {
	case err := <-served:
		require.ErrorIs(t, err, http.ErrServerClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("expected the HTTP server to stop")
	}
	_, err = os.Stat(config.Get().System.GetStatesPath())
	require.NoError(t, err, "expected the server states to be saved")

	// The serving goroutine waits on this channel before the process exits, so it
	// must be closed once the graceful shutdown has finished.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("expected shutdown to signal completion")
	}
}
