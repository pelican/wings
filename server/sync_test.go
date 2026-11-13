package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
)

// TestSyncWithConfigurationDoesNotHoldItsLock is a regression test for a
// deadlock that stopped Wings booting at all.
//
// SyncWithConfiguration takes the configuration write lock for its whole body.
// Code added inside it called PluginSnapshot, which takes the read lock, and
// because `go f(expr)` evaluates expr in the calling goroutine, the snapshot
// ran before anything was spawned. Every server deadlocked while the manager
// was loading them, the boot worker pool never drained, and the daemon sat
// there with nothing listening.
//
// Anything inside this function that reads the server's configuration brings
// that back, so this fails if the lock is still held when it returns.
func TestSyncWithConfigurationDoesNotHoldItsLock(t *testing.T) {
	// SyncWithConfiguration reads the global configuration, which a unit test
	// has none of unless one is set.
	config.Set(&config.Configuration{AuthenticationToken: "test-token"})

	s := &Server{}

	cfg := Configuration{}
	cfg.Uuid = "11111111-1111-1111-1111-111111111111"
	s.cfg = cfg

	// A minimal payload is enough; the point is the locking, not what gets
	// unmarshalled.
	response := remote.ServerConfigurationResponse{
		Settings:             json.RawMessage(`{"uuid":"11111111-1111-1111-1111-111111111111"}`),
		ProcessConfiguration: &remote.ProcessConfiguration{},
	}

	if err := s.SyncWithConfiguration(response); err != nil {
		t.Fatalf("SyncWithConfiguration: %v", err)
	}

	// Reading the configuration must not block now that it has returned.
	done := make(chan string, 1)
	go func() { done <- s.Config().Uuid }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the configuration lock is still held after SyncWithConfiguration returned, which is what deadlocked boot")
	}

	// PluginSnapshot is not called here because it also reads the environment,
	// which a bare Server has none of. Config is what it blocked on, and
	// Config is what this checks.
}
