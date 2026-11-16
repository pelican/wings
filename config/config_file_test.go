package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// newTestConfig writes a configuration file in a temp directory and returns
// the configuration.
func newTestConfig(t *testing.T) *Configuration {
	t.Helper()
	c, err := NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = "token"
	if err := WriteToDisk(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPluginsFromFileDropsEntriesNoLongerInTheFile(t *testing.T) {
	c := newTestConfig(t)
	c.Plugins.Registries = map[string]PluginRegistry{"ours": {URL: "https://a.example/registry.json"}}
	if err := WriteToDisk(c); err != nil {
		t.Fatal(err)
	}
	// In memory, a registry the file doesn't have (removed by hand, or added
	// by something that should not have).
	c.Plugins.Registries["stale"] = PluginRegistry{URL: "https://b.example/registry.json"}

	got, err := pluginsFromFile(c.path)
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

// TestWriteToDiskKeepingPluginsKeepsTheFilesBlock covers the daemon writing
// its copy of the configuration after `wings plugins disable` changed the file.
func TestWriteToDiskKeepingPluginsKeepsTheFilesBlock(t *testing.T) {
	c := newTestConfig(t)
	if _, err := UpdatePluginsOnDisk(c, func(p *PluginConfiguration) { p.Disabled = []string{"audit"} }); err != nil {
		t.Fatal(err)
	}
	c.Plugins.Disabled = nil // the daemon's copy from before the command ran
	c.Api.Port = 9090        // and a change the daemon is making

	if err := WriteToDiskKeepingPlugins(c); err != nil {
		t.Fatal(err)
	}
	got, err := pluginsFromFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Disabled, []string{"audit"}) || !slices.Equal(c.Plugins.Disabled, []string{"audit"}) {
		t.Fatalf("disabled = %v in the file, %v in memory; want [audit] in both", got.Disabled, c.Plugins.Disabled)
	}
}

// TestUpdatePluginsOnDiskFromSeveralProcesses runs `wings plugins disable`
// the way operators can: several at once, as separate processes. Every
// change must survive.
func TestUpdatePluginsOnDiskFromSeveralProcesses(t *testing.T) {
	const processes, changes = 4, 25
	if path := os.Getenv("WINGS_CONFIG_HELPER_PATH"); path != "" {
		c, err := NewAtPath(path)
		if err != nil {
			t.Fatal(err)
		}
		id := os.Getenv("WINGS_CONFIG_HELPER_ID")
		for i := 0; i < changes; i++ {
			name := fmt.Sprintf("p%s-%d", id, i)
			if _, err := UpdatePluginsOnDisk(c, func(p *PluginConfiguration) { p.Disabled = append(p.Disabled, name) }); err != nil {
				t.Fatal(err)
			}
		}
		return
	}

	c := newTestConfig(t)
	cmds := make([]*exec.Cmd, processes)
	for i := range cmds {
		cmds[i] = exec.Command(os.Args[0], "-test.run=^TestUpdatePluginsOnDiskFromSeveralProcesses$", "-test.count=1")
		cmds[i].Env = append(os.Environ(), "WINGS_CONFIG_HELPER_PATH="+c.path, "WINGS_CONFIG_HELPER_ID="+strconv.Itoa(i))
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper process: %v", err)
		}
	}
	got, err := pluginsFromFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Disabled) != processes*changes {
		t.Fatalf("%d of %d changes survived", len(got.Disabled), processes*changes)
	}
}
