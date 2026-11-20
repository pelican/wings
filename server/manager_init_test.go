package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pelican/wings/config"
)

// The configuration returned by the Panel must be for the server that was
// requested, and nothing may be created on the host when it is not
// (GHSA-324w-x2q6-3288).
func TestInitServerRejectsConfigurationForAnotherServer(t *testing.T) {
	data := t.TempDir()
	setTestConfig(t, func(c *config.Configuration) { c.System.Data = data })

	m := &Manager{}
	_, err := m.InitServer("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", serverConfiguration(t, testServerUuid, 1))
	if err == nil {
		t.Fatal("expected a configuration for another server to be rejected")
	}

	entries, err := os.ReadDir(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected nothing to be created in the data directory, found %v", entries)
	}
	if _, err := os.Stat(filepath.Join(data, testServerUuid)); !os.IsNotExist(err) {
		t.Fatalf("expected no data directory for the rejected server, got %v", err)
	}
}

// A server can only be added to the manager once.
func TestManagerAddIfMissing(t *testing.T) {
	a := newConfiguredServer(t)
	b, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncWithConfiguration(serverConfiguration(t, testServerUuid, 2)); err != nil {
		t.Fatal(err)
	}

	m := &Manager{}
	if !m.AddIfMissing(a) {
		t.Fatal("expected the first server to be added")
	}
	if m.AddIfMissing(b) {
		t.Fatal("expected a second server with the same id to be refused")
	}
	if n := m.Len(); n != 1 {
		t.Fatalf("expected one server in the manager, got %d", n)
	}
}
