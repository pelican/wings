package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pelican/wings/config"
)

func TestTrustStoreInvalidatesRuntimeScopeChanges(t *testing.T) {
	c := setConfig(t)
	c.Plugins.Directory = t.TempDir()
	config.Set(c)

	ts, err := LoadTrustStore()
	if err != nil {
		t.Fatalf("LoadTrustStore: %v", err)
	}
	base := trustTestManifest()
	if err := ts.Approve(base); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !ts.IsTrusted(base) {
		t.Fatal("approved manifest should be trusted")
	}

	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{
			name: "capability",
			mutate: func(m *Manifest) {
				m.Capability = Capability("other")
			},
		},
		{
			name: "provided events",
			mutate: func(m *Manifest) {
				m.Provides.Events = []string{"server.install"}
			},
		},
		{
			name: "entrypoint",
			mutate: func(m *Manifest) {
				m.Entrypoint = []string{"node", "other.js"}
			},
		},
		{
			name: "host api grants",
			mutate: func(m *Manifest) {
				m.Requires.HostAPI = append(m.Requires.HostAPI, "server.power")
			},
		},
		{
			name: "version",
			mutate: func(m *Manifest) {
				m.Version = "1.0.1"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			changed.Entrypoint = append([]string(nil), base.Entrypoint...)
			changed.Provides.Events = append([]string(nil), base.Provides.Events...)
			changed.Requires.HostAPI = append([]string(nil), base.Requires.HostAPI...)
			tt.mutate(&changed)
			if ts.IsTrusted(changed) {
				t.Fatal("changed manifest should require re-approval")
			}
		})
	}
}

func TestTrustStoreTreatsSetLikeFieldsAsOrderInsensitive(t *testing.T) {
	c := setConfig(t)
	c.Plugins.Directory = t.TempDir()
	config.Set(c)

	ts, err := LoadTrustStore()
	if err != nil {
		t.Fatalf("LoadTrustStore: %v", err)
	}
	base := trustTestManifest()
	base.Provides.Events = []string{"server.state", "server.install"}
	base.Requires.HostAPI = []string{"server.stats", "server.power"}
	if err := ts.Approve(base); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	reordered := base
	reordered.Provides.Events = []string{"server.install", "server.state"}
	reordered.Requires.HostAPI = []string{"server.power", "server.stats"}
	if !ts.IsTrusted(reordered) {
		t.Fatal("reordering set-like manifest fields should not invalidate trust")
	}

	entrypointReordered := base
	entrypointReordered.Entrypoint = []string{"index.js", "node"}
	if ts.IsTrusted(entrypointReordered) {
		t.Fatal("reordering entrypoint should invalidate trust")
	}
}

func TestTrustStoreInvalidatesContentChanges(t *testing.T) {
	c := setConfig(t)
	c.Plugins.Directory = t.TempDir()
	config.Set(c)

	pdir := filepath.Join(c.Plugins.Directory, "audit")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "" +
		"apiVersion: v1\n" +
		"name: audit\n" +
		"capability: events\n" +
		"version: 1.0.0\n" +
		"entrypoint: [\"node\", \"index.js\"]\n" +
		"provides:\n" +
		"  events: [\"server.state\"]\n"
	if err := os.WriteFile(filepath.Join(pdir, ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(pdir, "index.js")
	if err := os.WriteFile(entrypoint, []byte("console.log('v1')\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	m, err := LoadManifest(filepath.Join(pdir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := LoadTrustStore()
	if err != nil {
		t.Fatalf("LoadTrustStore: %v", err)
	}
	if err := ts.Approve(m); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !ts.IsTrusted(m) {
		t.Fatal("approved manifest should be trusted")
	}

	if err := os.WriteFile(entrypoint, []byte("console.log('v2')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ts.IsTrusted(m) {
		t.Fatal("changed plugin content should require re-approval")
	}
}

func trustTestManifest() Manifest {
	return Manifest{
		APIVersion: "v1",
		Name:       "audit",
		Capability: CapabilityEvents,
		Version:    "1.0.0",
		Entrypoint: []string{"node", "index.js"},
		Provides: Provides{
			Events: []string{"server.state"},
		},
		Requires: Requires{
			HostAPI: []string{"server.stats"},
		},
	}
}
