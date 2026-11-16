package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const settingsManifest = `apiVersion: v1
name: notify
capability: events
entrypoint: ["./notify"]
settings:
  - key: webhook_url
    label: Webhook URL
    type: string
    required: true
    secret: true
  - key: level
    type: select
    options: [info, warn, {value: error, label: Errors only}]
    default: warn
  - key: retries
    type: number
    default: 3
  - key: verbose
    type: boolean
`

// writeManifest puts a plugin.yaml for the plugin in a new plugins directory
// and loads it.
func writeSettingsManifest(t *testing.T, body string) Manifest {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "notify")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	return m
}

func TestManifestReadsTheSettingsSchema(t *testing.T) {
	m := writeSettingsManifest(t, settingsManifest)
	if len(m.Settings) != 4 {
		t.Fatalf("got %d settings, want 4", len(m.Settings))
	}
	level := m.Settings[1]
	if level.Options[0] != (SettingOption{Value: "info", Label: "info"}) || level.Options[2] != (SettingOption{Value: "error", Label: "Errors only"}) {
		t.Fatalf("select options = %+v", level.Options)
	}
	if m.Settings[2].Default != float64(3) {
		t.Fatalf("a numeric default should be read as a float64, got %T %v", m.Settings[2].Default, m.Settings[2].Default)
	}
	if m.Settings[3].Type != SettingBoolean || m.Settings[0].Type != SettingString {
		t.Fatalf("types = %q %q", m.Settings[0].Type, m.Settings[3].Type)
	}
}

func TestManifestRefusesABadSettingsSchema(t *testing.T) {
	base := "apiVersion: v1\nname: notify\ncapability: events\nentrypoint: [\"./notify\"]\nsettings:\n"
	tests := map[string]string{
		"unknown type":        "  - {key: a, type: color}\n",
		"duplicate key":       "  - {key: a}\n  - {key: a}\n",
		"empty key":           "  - {type: string}\n",
		"key with slash":      "  - {key: a/b}\n",
		"key with a dot":      "  - {key: a.b}\n",
		"key starting with -": "  - {key: -a}\n",
		"select without opts": "  - {key: a, type: select}\n",
		"options on a string": "  - {key: a, options: [x]}\n",
		"default wrong type":  "  - {key: a, type: number, default: lots}\n",
		"default not an opt":  "  - {key: a, type: select, options: [x], default: y}\n",
		"secret number":       "  - {key: a, type: number, secret: true}\n",
	}
	for name, settings := range tests {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "notify")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(base+settings), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadManifest(filepath.Join(dir, ManifestFile)); err == nil {
				t.Fatal("expected the manifest to be refused")
			}
		})
	}
}

func TestValidateSettings(t *testing.T) {
	m := writeSettingsManifest(t, settingsManifest)
	ok := map[string]any{"webhook_url": "https://example.test/hook", "level": "error", "retries": float64(5), "verbose": true}
	if err := ValidateSettings(m, ok); err != nil {
		t.Fatalf("valid values refused: %v", err)
	}
	tests := []struct {
		name   string
		values map[string]any
		key    string
	}{
		{"unknown key", map[string]any{"webhook_url": "x", "colour": "red"}, "colour"},
		{"string for a number", map[string]any{"webhook_url": "x", "retries": "5"}, "retries"},
		{"number for a boolean", map[string]any{"webhook_url": "x", "verbose": float64(1)}, "verbose"},
		{"select value not an option", map[string]any{"webhook_url": "x", "level": "debug"}, "level"},
		{"number for a string", map[string]any{"webhook_url": float64(1)}, "webhook_url"},
		{"required missing", map[string]any{}, "webhook_url"},
		{"required empty", map[string]any{"webhook_url": ""}, "webhook_url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSettings(m, tt.values)
			se, isSE := err.(SettingsError)
			if !isSE || se[tt.key] == "" {
				t.Fatalf("want a SettingsError naming %q, got %v", tt.key, err)
			}
		})
	}
}

func TestSaveSettingsWritesAPrivateFileInTheDataDirectory(t *testing.T) {
	m := writeSettingsManifest(t, settingsManifest)
	values := map[string]any{"webhook_url": "https://example.test/hook", "retries": float64(2)}
	if err := SaveSettings(m, values); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	path := filepath.Join(m.DataDir(), SettingsFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("settings file mode = %o, want 600", perm)
	}
	b, _ := os.ReadFile(path)
	var onDisk map[string]any
	if err := json.Unmarshal(b, &onDisk); err != nil || onDisk["webhook_url"] != "https://example.test/hook" || onDisk["retries"] != float64(2) {
		t.Fatalf("settings file = %s (%v)", b, err)
	}
	got, err := LoadSettings(m)
	if err != nil || got["retries"] != float64(2) {
		t.Fatalf("LoadSettings = %v, %v", got, err)
	}
	// No temporary file is left next to it.
	entries, _ := os.ReadDir(m.DataDir())
	if len(entries) != 1 {
		t.Fatalf("data directory has %d entries, want only %s", len(entries), SettingsFile)
	}

	if err := SaveSettings(m, map[string]any{"webhook_url": "x", "nope": true}); err == nil {
		t.Fatal("an unknown key was saved")
	}
	if got, _ := LoadSettings(m); got["nope"] != nil {
		t.Fatal("a refused save changed the file")
	}
}

func TestLoadSettingsWithoutAFileIsEmpty(t *testing.T) {
	m := writeSettingsManifest(t, settingsManifest)
	got, err := LoadSettings(m)
	if err != nil || len(got) != 0 {
		t.Fatalf("LoadSettings = %v, %v; want empty", got, err)
	}
}

func TestUpdateSettingsKeepsMaskedSecretsAndUnsetsNulls(t *testing.T) {
	m := writeSettingsManifest(t, settingsManifest)
	if err := SaveSettings(m, map[string]any{"webhook_url": "https://secret.test/abc", "verbose": true}); err != nil {
		t.Fatal(err)
	}
	shown := MaskSecrets(m, mustLoad(t, m))
	if shown["webhook_url"] != SecretMask || shown["verbose"] != true {
		t.Fatalf("masked values = %v", shown)
	}
	// What a form shows comes back unchanged, plus edits.
	shown["verbose"] = nil
	shown["level"] = "info"
	saved, err := UpdateSettings(m, shown)
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if saved["webhook_url"] != "https://secret.test/abc" {
		t.Fatalf("the masked secret was not kept: %v", saved)
	}
	if _, ok := saved["verbose"]; ok {
		t.Fatalf("a null should unset the key: %v", saved)
	}
	if got := mustLoad(t, m); got["level"] != "info" || got["webhook_url"] != "https://secret.test/abc" {
		t.Fatalf("stored = %v", got)
	}

	// A new secret value replaces the old one.
	if _, err := UpdateSettings(m, map[string]any{"webhook_url": "https://new.test"}); err != nil {
		t.Fatal(err)
	}
	if got := mustLoad(t, m); got["webhook_url"] != "https://new.test" {
		t.Fatalf("stored = %v", got)
	}
}

func TestSaveSettingsDoesNotFollowASymlinkLeavingThePluginsDirectory(t *testing.T) {
	m := writeSettingsManifest(t, settingsManifest)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(m.DataDir()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, m.DataDir()); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(m, map[string]any{"webhook_url": "x"}); err == nil {
		t.Fatal("expected the write through a symlink leaving the plugins directory to fail")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote %d entries outside the plugins directory", len(entries))
	}
}

func mustLoad(t *testing.T, m Manifest) map[string]any {
	t.Helper()
	v, err := LoadSettings(m)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Values that each pass validation can still add up, once encoded, to more
// than LoadSettings reads. Saving them must fail and keep the stored values,
// or the plugin's settings could never be read or repaired again.
func TestSaveSettingsRefusesAFileOverTheCap(t *testing.T) {
	var body strings.Builder
	body.WriteString("apiVersion: v1\nname: notify\ncapability: events\nentrypoint: [\"./notify\"]\nsettings:\n")
	values := map[string]any{}
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("s%d", i)
		fmt.Fprintf(&body, "  - key: %s\n    type: string\n", key)
		values[key] = strings.Repeat("x", maxSettingValueBytes)
	}
	m := writeSettingsManifest(t, body.String())
	if err := SaveSettings(m, map[string]any{"s0": "kept"}); err != nil {
		t.Fatal(err)
	}
	err := SaveSettings(m, values)
	if !errors.Is(err, ErrSettingsTooLarge) {
		t.Fatalf("SaveSettings = %v, want ErrSettingsTooLarge", err)
	}
	if got := mustLoad(t, m); got["s0"] != "kept" {
		t.Fatalf("stored values = %v, want the previous ones kept", got)
	}
	if _, err := UpdateSettings(m, values); !errors.Is(err, ErrSettingsTooLarge) {
		t.Fatalf("UpdateSettings = %v, want ErrSettingsTooLarge", err)
	}
}
