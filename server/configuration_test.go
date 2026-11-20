package server

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
)

const testServerUuid = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func setTestConfig(t *testing.T, modify ...func(*config.Configuration)) {
	t.Helper()
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationToken = "test-token"
	for _, fn := range modify {
		fn(cfg)
	}
	config.Set(cfg)
}

func newConfiguredServer(t *testing.T) *Server {
	t.Helper()
	setTestConfig(t)

	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncWithConfiguration(serverConfiguration(t, testServerUuid, 1)); err != nil {
		t.Fatal(err)
	}
	return s
}

func serverConfiguration(t *testing.T, uuid string, disk int64) remote.ServerConfigurationResponse {
	t.Helper()
	settings, err := json.Marshal(map[string]any{
		"uuid":  uuid,
		"build": map[string]any{"disk_space": disk},
	})
	if err != nil {
		t.Fatal(err)
	}
	return remote.ServerConfigurationResponse{
		Settings:             settings,
		ProcessConfiguration: &remote.ProcessConfiguration{},
	}
}

// The UUID returned by the Panel is used to build paths on the host, so only a
// real UUID may be accepted (GHSA-324w-x2q6-3288).
func TestSyncWithConfigurationRejectsInvalidUuid(t *testing.T) {
	setTestConfig(t)

	for _, uuid := range []string{"", "..", "../escaped", "../wings-audit-probe-aaaaaaaaaaaaaaa", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa/"} {
		s, err := New(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SyncWithConfiguration(serverConfiguration(t, uuid, 1)); err == nil {
			t.Errorf("expected uuid %q to be rejected", uuid)
		}
		if s.ID() != "" {
			t.Errorf("expected rejected uuid %q not to be stored, got %q", uuid, s.ID())
		}
	}
}

// Once a server exists, a configuration for a different server must not change
// its identity.
func TestSyncWithConfigurationRejectsDifferentServer(t *testing.T) {
	s := newConfiguredServer(t)

	err := s.SyncWithConfiguration(serverConfiguration(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", 2))
	if err == nil {
		t.Fatal("expected a configuration for another server to be rejected")
	}
	if s.ID() != testServerUuid {
		t.Fatalf("expected the server id to stay %q, got %q", testServerUuid, s.ID())
	}
	if d := s.DiskSpace(); d != 1024*1024 {
		t.Fatalf("expected the configuration to be unchanged, got disk space %d", d)
	}
}

// Syncing replaces the configuration while other goroutines may be waiting to
// read it, and none of them should be left waiting forever.
func TestSyncWithConfigurationWhileReading(t *testing.T) {
	s := newConfiguredServer(t)
	responses := []remote.ServerConfigurationResponse{
		serverConfiguration(t, testServerUuid, 1),
		serverConfiguration(t, testServerUuid, 2),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				for j := 0; j < 500; j++ {
					if err := s.SyncWithConfiguration(responses[j%2]); err != nil {
						t.Error(err)
						return
					}
				}
			}()
			go func() {
				defer wg.Done()
				for j := 0; j < 500; j++ {
					_ = s.DiskSpace()
					_ = s.Config().GetUuid()
					_ = s.cfg.snapshot()
				}
			}()
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(time.Second * 30):
		t.Fatal("timed out waiting for configuration readers and writers to finish")
	}
	if d := s.DiskSpace(); d != 1024*1024 && d != 2*1024*1024 {
		t.Fatalf("expected disk space from one of the synced configurations, got %d", d)
	}
}

// The configuration is returned by the API, so it must keep encoding its
// values at the top level of the object.
func TestConfigurationSnapshotJSON(t *testing.T) {
	s := newConfiguredServer(t)

	b, err := json.Marshal(s.cfg.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if string(v["uuid"]) != `"`+testServerUuid+`"` {
		t.Fatalf("expected uuid at the top level of the configuration, got %s", b)
	}
	if _, ok := v["build"]; !ok {
		t.Fatalf("expected build at the top level of the configuration, got %s", b)
	}
}
