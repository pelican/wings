package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v2"
)

func loadLogConfig(t *testing.T, doc string) DockerLogConfiguration {
	t.Helper()
	c, err := NewAtPath("")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(doc), c); err != nil {
		t.Fatal(err)
	}
	return c.Docker.LogConfig
}

func TestDockerLogConfiguration_UnmarshalYAML(t *testing.T) {
	defaults := map[string]string{"max-size": "5m", "max-file": "1", "compress": "false", "mode": "non-blocking"}

	t.Run("defaults when unset", func(t *testing.T) {
		got := loadLogConfig(t, "docker: {}\n")
		if got.Type != "local" || !reflect.DeepEqual(got.Config, defaults) {
			t.Fatalf("unexpected defaults: %+v", got)
		}
	})

	t.Run("other driver does not inherit local defaults", func(t *testing.T) {
		got := loadLogConfig(t, `
docker:
  log_config:
    type: loki
    config:
      loki-url: http://127.0.0.1:3100/loki/api/v1/push
      loki-retries: 5
`)
		want := map[string]string{"loki-url": "http://127.0.0.1:3100/loki/api/v1/push", "loki-retries": "5"}
		if got.Type != "loki" || !reflect.DeepEqual(got.Config, want) {
			t.Fatalf("expected only the configured loki options, got %+v", got)
		}
	})

	t.Run("other driver without options", func(t *testing.T) {
		got := loadLogConfig(t, "docker:\n  log_config:\n    type: fluentd\n")
		if got.Type != "fluentd" || len(got.Config) != 0 {
			t.Fatalf("expected fluentd without options, got %+v", got)
		}
	})

	t.Run("local driver merges with defaults", func(t *testing.T) {
		got := loadLogConfig(t, "docker:\n  log_config:\n    config:\n      max-size: 10m\n")
		want := map[string]string{"max-size": "10m", "max-file": "1", "compress": "false", "mode": "non-blocking"}
		if got.Type != "local" || !reflect.DeepEqual(got.Config, want) {
			t.Fatalf("expected merged local options, got %+v", got)
		}
	})

	t.Run("survives a write and reload", func(t *testing.T) {
		first := loadLogConfig(t, "docker:\n  log_config:\n    type: fluentd\n    config:\n      fluentd-address: localhost:24224\n")
		c, err := NewAtPath("")
		if err != nil {
			t.Fatal(err)
		}
		c.Docker.LogConfig = first
		b, err := yaml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		second := loadLogConfig(t, string(b))
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("config changed after round trip: %+v -> %+v", first, second)
		}
	})
}
