package config

import (
	"path/filepath"
	"testing"
)

func TestPluginsOnDiskDropsEntriesNoLongerInTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	c, err := NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = "token"
	c.Plugins.Registries = map[string]PluginRegistry{"ours": {URL: "https://a.example/registry.json"}}
	if err := WriteToDisk(c); err != nil {
		t.Fatal(err)
	}
	// In memory, a registry the file doesn't have (removed by hand, or added
	// by something that should not have).
	c.Plugins.Registries["stale"] = PluginRegistry{URL: "https://b.example/registry.json"}

	got, err := PluginsOnDisk(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Registries["stale"]; ok {
		t.Fatal("a registry that is not in the file was kept")
	}
	if _, ok := got.Registries["ours"]; !ok || !got.Autoload || got.Directory == "" {
		t.Fatalf("lost the file's values or the defaults: %+v", got)
	}
}
