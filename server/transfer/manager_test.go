package transfer

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server"
)

func newManagerTestServer(t *testing.T) *server.Server {
	t.Helper()
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationToken = "test-token"
	config.Set(cfg)

	s, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]string{"uuid": "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"})
	if err := s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: settings, ProcessConfiguration: &remote.ProcessConfiguration{}}); err != nil {
		t.Fatal(err)
	}
	return s
}

// A transfer that finishes after another transfer of the same server has
// replaced it must not remove the newer transfer.
func TestRemoveOnlyRemovesItsOwnTransfer(t *testing.T) {
	s := newManagerTestServer(t)
	m := NewManager()

	first := &Transfer{Server: s}
	second := &Transfer{Server: s}
	m.Add(first)
	m.Add(second)

	m.Remove(first)
	if got := m.Get(s.ID()); got != second {
		t.Fatal("expected removing the replaced transfer to keep the newer one")
	}

	m.Remove(second)
	if got := m.Get(s.ID()); got != nil {
		t.Fatal("expected the transfer to be removed")
	}
}
