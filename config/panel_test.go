package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func newPanelTestConfig(t *testing.T) *Configuration {
	t.Helper()
	c, err := NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func applyPanelJSON(t *testing.T, c *Configuration, body string) error {
	t.Helper()
	var p PanelConfiguration
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("failed to decode %s: %v", body, err)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	c.ApplyPanelConfiguration(p)
	return nil
}

func configYAML(t *testing.T, c *Configuration) string {
	t.Helper()
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// nest turns a dotted key and a value into the nested JSON object the Panel sends.
func nest(key string, value any) string {
	parts := strings.Split(key, ".")
	var v any = value
	for i := len(parts) - 1; i >= 0; i-- {
		v = map[string]any{parts[i]: v}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Every key the Panel sends must change the configuration. A key that does not
// match a field would be silently ignored. The keys are the ones the Panel always
// sends plus Node::DAEMON_CONFIG_KEYS.
func TestPanelConfigurationAppliesEveryPanelKey(t *testing.T) {
	keys := map[string]any{
		"uuid":                                 "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		"token_id":                             "new-token-id",
		"token":                                "new-token",
		"api.host":                             "127.0.0.1",
		"api.port":                             8443,
		"api.ssl.enabled":                      true,
		"api.ssl.cert":                         "/etc/ssl/custom/fullchain.pem",
		"api.ssl.key":                          "/etc/ssl/custom/privkey.pem",
		"api.upload_limit":                     512,
		"system.sftp.bind_port":                2222,
		"debug":                                true,
		"app_name":                             "Custom",
		"allowed_origins":                      []string{"https://example.com"},
		"allow_cors_private_network":           true,
		"api.trusted_proxies":                  []string{"10.0.0.0/8", "192.0.2.1"},
		"api.remote_download.max_redirects":    3,
		"remote_query.timeout":                 45,
		"system.username":                      "gameserver",
		"system.timezone":                      "UTC",
		"system.quotas.enabled":                true,
		"system.sftp.bind_address":             "127.0.0.1",
		"system.user.rootless.enabled":         true,
		"system.user.passwd.enable":            false,
		"system.machine_id.enable":             false,
		"docker.network.name":                  "custom_nw",
		"docker.network.driver":                "macvlan",
		"docker.network.interface":             "172.20.0.1",
		"docker.network.dns":                   []string{"9.9.9.9"},
		"docker.network.ispn":                  true,
		"docker.network.IPv6":                  false,
		"docker.network.interfaces.v4.subnet":  "172.20.0.0/16",
		"docker.network.interfaces.v4.gateway": "172.20.0.1",
		"docker.network.interfaces.v6.subnet":  "fd00:1::/64",
		"docker.network.interfaces.v6.gateway": "fd00:1::1",
		"docker.domainname":                    "example.com",
		"docker.tmpfs_size":                    200,
		"docker.container_pid_limit":           1024,
		"docker.installer_limits.memory":       2048,
		"docker.installer_limits.cpu":          200,
		"docker.overhead.override":             true,
		"docker.overhead.default_multiplier":   1.5,
		"docker.use_performant_inspect":        false,
		"docker.userns_mode":                   "host",
		"docker.system_ips":                    []string{"192.0.2.10"},
		"docker.log_config.type":               "json-file",
		"throttles.enabled":                    false,
		"throttles.lines":                      5000,
		"throttles.line_reset_interval":        250,
	}

	for key, value := range keys {
		t.Run(key, func(t *testing.T) {
			c := newPanelTestConfig(t)
			before := configYAML(t, c)
			if err := applyPanelJSON(t, c, nest(key, value)); err != nil {
				t.Fatalf("expected %s to be accepted: %v", key, err)
			}
			if configYAML(t, c) == before {
				t.Fatalf("expected %s to change the configuration", key)
			}
		})
	}
}

// Values that control how Wings runs on the host can only be set in config.yml,
// so they are ignored when sent by the Panel (GHSA-8rw4-fw83-3rv7).
func TestPanelConfigurationIgnoresHostSettings(t *testing.T) {
	bodies := []string{
		`{"system":{"user":{"uid":0,"gid":0}}}`,
		`{"system":{"user":{"passwd":{"directory":"/etc"}}}}`,
		`{"system":{"user":{"rootless":{"container_uid":0}}}}`,
		`{"system":{"machine_id":{"directory":"/etc"}}}`,
		`{"system":{"root_directory":"/","data":"/","log_directory":"/","tmp_directory":"/","backup_directory":"/","archive_directory":"/"}}`,
		`{"system":{"backups":{"restore_host_allowlist":["127.0.0.1"]}}}`,
		`{"system":{"sftp":{"read_only":true,"key_only":true}}}`,
		`{"docker":{"network":{"network_mode":"host","mode":"host","is_internal":true}}}`,
		`{"docker":{"registries":{"registry.example.com":{"username":"u","password":"p"}}}}`,
		`{"docker":{"log_config":{"config":{"max-size":"100g"}}}}`,
		`{"allowed_mounts":["/"],"remote":"http://attacker.example.com","ignore_panel_config_updates":true}`,
		`{"api":{"disable_remote_download":true}}`,
		`{"System":{"User":{"Uid":0}},"Docker":{"Network":{"Mode":"host"}}}`,
	}

	for _, body := range bodies {
		c := newPanelTestConfig(t)
		before := configYAML(t, c)
		if err := applyPanelJSON(t, c, body); err != nil {
			t.Fatalf("expected %s to be accepted and ignored: %v", body, err)
		}
		if configYAML(t, c) != before {
			t.Errorf("expected %s not to change the configuration", body)
		}
	}
}

func TestPanelConfigurationRejectsInvalidValues(t *testing.T) {
	invalid := map[string]any{
		"system.username":                      "pelican --uid 0",
		"docker.userns_mode":                   "private",
		"docker.network.name":                  "bad name",
		"docker.network.interface":             "not-an-ip",
		"docker.network.dns":                   []string{"1.1.1.1", "evil"},
		"docker.network.interfaces.v4.subnet":  "fd00::/64",
		"docker.network.interfaces.v6.gateway": "10.0.0.1",
		"docker.system_ips":                    []string{"host"},
		"docker.log_config.type":               "local --privileged",
		"docker.domainname":                    "bad domain",
		"docker.overhead.default_multiplier":   1000,
		"docker.container_pid_limit":           -1,
		"api.ssl.cert":                         "relative/fullchain.pem",
		"api.ssl.key":                          "/etc/../etc/shadow",
		"api.port":                             70000,
		"api.host":                             "bad host",
		"api.trusted_proxies":                  []string{"anywhere"},
		"system.sftp.bind_address":             "0.0.0.0 ",
		"system.sftp.bind_port":                0,
		"system.timezone":                      "../../etc/passwd",
		"uuid":                                 "../escaped",
		"app_name":                             "bad\x1b[31m",
	}

	for key, value := range invalid {
		t.Run(key, func(t *testing.T) {
			c := newPanelTestConfig(t)
			before := configYAML(t, c)
			if err := applyPanelJSON(t, c, nest(key, value)); err == nil {
				t.Fatalf("expected %s=%v to be rejected", key, value)
			}
			if configYAML(t, c) != before {
				t.Fatalf("expected a rejected update not to change the configuration")
			}
		})
	}
}

// Applying an update must not share slices with the configuration it was copied
// from, since Get returns a shallow copy of the global configuration.
func TestPanelConfigurationCopiesSlices(t *testing.T) {
	c := newPanelTestConfig(t)
	c.Docker.Network.Dns = make([]string, 1, 4)
	c.Docker.Network.Dns[0] = "1.1.1.1"
	original := c.Docker.Network.Dns

	copied := *c
	if err := applyPanelJSON(t, &copied, `{"docker":{"network":{"dns":["9.9.9.9","8.8.8.8"]}}}`); err != nil {
		t.Fatal(err)
	}
	if original[0] != "1.1.1.1" || len(original) != 1 {
		t.Fatalf("expected the original configuration to be unchanged, got %v", original)
	}
}
