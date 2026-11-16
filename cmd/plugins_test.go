package cmd

import (
	"path/filepath"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// usePluginPolicy installs a configuration with the given plugin policy and
// returns it, so a test can apply togglePlugin and ask plugin.Enabled.
func usePluginPolicy(t *testing.T, autoload bool, enabled, disabled []string) *config.Configuration {
	t.Helper()
	c, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = "test-token"
	c.Plugins.Autoload = autoload
	c.Plugins.Enabled = enabled
	c.Plugins.Disabled = disabled
	config.Set(c)
	return c
}

func TestEnableWithAutoloadOffAddsToAllowlist(t *testing.T) {
	c := usePluginPolicy(t, false, nil, nil)
	togglePlugin(&c.Plugins, "foo", true)
	config.Set(c)

	if !plugin.Enabled("foo") {
		t.Fatalf("foo should be enabled after `plugins enable foo` with autoload off; policy %+v", c.Plugins)
	}
	if plugin.Enabled("bar") {
		t.Fatal("enabling foo must not enable anything else")
	}
}

func TestDisablingTheLastAllowlistedPluginKeepsTheAllowlist(t *testing.T) {
	c := usePluginPolicy(t, true, []string{"only"}, nil)
	togglePlugin(&c.Plugins, "only", false)
	config.Set(c)

	if plugin.Enabled("only") {
		t.Fatal("the disabled plugin should be off")
	}
	if plugin.Enabled("other") {
		t.Fatalf("disabling the last allowlisted plugin turned other plugins on; policy %+v", c.Plugins)
	}

	togglePlugin(&c.Plugins, "only", true)
	config.Set(c)
	if !plugin.Enabled("only") || plugin.Enabled("other") {
		t.Fatalf("re-enabling should restore exactly the allowlist; policy %+v", c.Plugins)
	}
}
