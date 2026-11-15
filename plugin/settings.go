package plugin

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// SettingType is the kind of value a plugin setting holds.
type SettingType string

const (
	SettingString  SettingType = "string"
	SettingNumber  SettingType = "number"
	SettingBoolean SettingType = "boolean"
	// SettingSelect is a string that must be one of the setting's options.
	SettingSelect SettingType = "select"
)

// Setting is one entry of a manifest's settings schema:
//
//	settings:
//	  - key: webhook_url
//	    label: Webhook URL
//	    type: string
//	    required: true
//	    secret: true
//	  - key: level
//	    type: select
//	    options: [info, warn, error]   # or [{value: info, label: Info}, ...]
//	    default: warn
type Setting struct {
	Key         string          `yaml:"key" json:"key"`
	Label       string          `yaml:"label" json:"label,omitempty"`
	Description string          `yaml:"description" json:"description,omitempty"`
	Type        SettingType     `yaml:"type" json:"type"`
	Options     []SettingOption `yaml:"options" json:"options,omitempty"`
	Default     any             `yaml:"default" json:"default,omitempty"`
	Required    bool            `yaml:"required" json:"required"`
	// Secret values are write-only through the Panel API: they are shown as
	// SecretMask, and sending SecretMask back keeps the stored value.
	Secret bool `yaml:"secret" json:"secret"`
}

// SettingOption is one choice of a select setting. In plugin.yaml it is
// either a plain string (used as value and label) or {value, label}.
type SettingOption struct {
	Value string `yaml:"value" json:"value"`
	Label string `yaml:"label" json:"label"`
}

// UnmarshalYAML accepts a scalar or a {value, label} mapping.
func (o *SettingOption) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		o.Value, o.Label = n.Value, n.Value
		return nil
	}
	type plain SettingOption
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*o = SettingOption(p)
	if o.Label == "" {
		o.Label = o.Value
	}
	return nil
}

// SettingsFile is the file in a plugin's data directory that holds its
// setting values, a JSON object keyed by setting key.
const SettingsFile = "settings.json"

// SecretMask stands in for a secret setting's value when settings are shown.
const SecretMask = "********"

const (
	maxSettingsFileBytes  = 1 << 20  // settings.json
	maxSettingValueBytes  = 64 << 10 // one string value
	maxSettingsPerPlugin  = 200
	maxSettingKeyLength   = 64
	maxSettingOptionCount = 500
)

// settingKeyPattern is the contract's (A3.1). No dots: the Panel's form
// library reads a dot in a field name as nesting.
var settingKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

// validateSettingsSchema checks a manifest's settings list. It also turns
// numeric defaults into float64, the type JSON numbers decode to, so a default
// and a stored value compare and print alike; the slice is changed in place.
func validateSettingsSchema(settings []Setting) error {
	if len(settings) > maxSettingsPerPlugin {
		return fmt.Errorf("more than %d settings", maxSettingsPerPlugin)
	}
	seen := make(map[string]bool, len(settings))
	for i := range settings {
		s := &settings[i]
		if s.Key == "" || len(s.Key) > maxSettingKeyLength || !settingKeyPattern.MatchString(s.Key) {
			return fmt.Errorf("setting %d has an invalid key %q (letters, digits, _ and -, not starting with -; at most %d characters)", i, s.Key, maxSettingKeyLength)
		}
		if seen[s.Key] {
			return fmt.Errorf("setting %q is declared twice", s.Key)
		}
		seen[s.Key] = true
		if s.Type == "" {
			s.Type = SettingString
		}
		switch s.Type {
		case SettingString, SettingNumber, SettingBoolean:
			if len(s.Options) > 0 {
				return fmt.Errorf("setting %q: options are only for type select", s.Key)
			}
		case SettingSelect:
			if len(s.Options) == 0 {
				return fmt.Errorf("setting %q: a select needs options", s.Key)
			}
			if len(s.Options) > maxSettingOptionCount {
				return fmt.Errorf("setting %q: more than %d options", s.Key, maxSettingOptionCount)
			}
			values := make(map[string]bool, len(s.Options))
			for _, o := range s.Options {
				if o.Value == "" || values[o.Value] {
					return fmt.Errorf("setting %q: option values must be set and distinct", s.Key)
				}
				values[o.Value] = true
			}
		default:
			return fmt.Errorf("setting %q has unknown type %q (want string, number, boolean or select)", s.Key, s.Type)
		}
		if s.Secret && s.Type != SettingString {
			return fmt.Errorf("setting %q: only string settings can be secret", s.Key)
		}
		if s.Default != nil {
			s.Default = normalizeNumber(s.Default)
			if err := checkSettingValue(*s, s.Default); err != nil {
				return fmt.Errorf("default: %w", err)
			}
		}
	}
	return nil
}

func normalizeNumber(v any) any {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f
		}
	}
	return v
}

// checkSettingValue reports whether v is a valid value for s.
func checkSettingValue(s Setting, v any) error {
	switch s.Type {
	case SettingString, "":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("setting %q must be a string", s.Key)
		}
		if len(str) > maxSettingValueBytes {
			return fmt.Errorf("setting %q is longer than %d bytes", s.Key, maxSettingValueBytes)
		}
	case SettingNumber:
		f, ok := normalizeNumber(v).(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("setting %q must be a number", s.Key)
		}
	case SettingBoolean:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("setting %q must be true or false", s.Key)
		}
	case SettingSelect:
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("setting %q must be one of its options", s.Key)
		}
		for _, o := range s.Options {
			if o.Value == str {
				return nil
			}
		}
		return fmt.Errorf("setting %q must be one of its options, not %q", s.Key, str)
	}
	return nil
}

// ErrSettingsTooLarge means the values, encoded, are larger than a settings
// file may be. Nothing was written.
var ErrSettingsTooLarge = errors.New("plugin: the settings are too large to store")

// SettingsError lists what is wrong with a set of values, by setting key.
type SettingsError map[string]string

func (e SettingsError) Error() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, e[k])
	}
	return "plugin: invalid settings: " + strings.Join(parts, "; ")
}

// ValidateSettings checks values against m's schema: every key must be
// declared, every value must have the declared type, and every required
// setting must have a value here or a default. Errors are a SettingsError.
func ValidateSettings(m Manifest, values map[string]any) error {
	bad := SettingsError{}
	for k, v := range values {
		s, ok := m.setting(k)
		if !ok {
			bad[k] = fmt.Sprintf("unknown setting %q", k)
			continue
		}
		if err := checkSettingValue(s, v); err != nil {
			bad[k] = err.Error()
		}
	}
	for _, s := range m.Settings {
		if !s.Required {
			continue
		}
		v, ok := values[s.Key]
		if !ok {
			v = s.Default
		}
		if v == nil || v == "" {
			if _, already := bad[s.Key]; !already {
				bad[s.Key] = fmt.Sprintf("setting %q is required", s.Key)
			}
		}
	}
	if len(bad) > 0 {
		return bad
	}
	return nil
}

func (m Manifest) setting(key string) (Setting, bool) {
	for _, s := range m.Settings {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

// MaskSecrets returns the declared settings' values with every non-empty
// secret replaced by SecretMask. Stored keys the schema no longer declares
// are left out.
func MaskSecrets(m Manifest, values map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	for _, s := range m.Settings {
		v, ok := values[s.Key]
		if !ok {
			continue
		}
		if s.Secret && v != nil && v != "" {
			v = SecretMask
		}
		out[s.Key] = v
	}
	return out
}

// settingsMu serializes read-modify-write of settings files in this process.
// The daemon is the only writer; the file is replaced by a rename, so readers
// (the plugin) never see a partial file.
var settingsMu sync.Mutex

// settingsLocation opens the plugins directory as an os.Root and returns it
// with the plugin's data directory and settings file relative to it, so a
// symlink planted in the plugins directory can't redirect the write.
func settingsLocation(m Manifest) (*os.Root, string, string, error) {
	if m.dir == "" || InvalidName(m.Name) {
		return nil, "", "", errors.New("plugin: settings need a manifest loaded from the plugins directory")
	}
	root, err := os.OpenRoot(filepath.Dir(m.dir))
	if err != nil {
		return nil, "", "", err
	}
	dataDir := path.Join(DataDirName, m.Name)
	return root, dataDir, path.Join(dataDir, SettingsFile), nil
}

// LoadSettings returns the values stored for m, or an empty map when none
// have been saved.
func LoadSettings(m Manifest) (map[string]any, error) {
	root, _, file, err := settingsLocation(m)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readSettings(root, file)
}

func readSettings(root *os.Root, file string) (map[string]any, error) {
	f, err := root.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSettingsFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSettingsFileBytes {
		return nil, fmt.Errorf("plugin: %s is larger than %d bytes", SettingsFile, maxSettingsFileBytes)
	}
	values := map[string]any{}
	if err := json.Unmarshal(b, &values); err != nil {
		return nil, fmt.Errorf("plugin: reading %s: %w", SettingsFile, err)
	}
	if values == nil {
		values = map[string]any{}
	}
	return values, nil
}

// SaveSettings validates values and replaces m's settings file with them.
func SaveSettings(m Manifest, values map[string]any) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	return saveSettingsLocked(m, values)
}

// UpdateSettings replaces m's stored values with incoming and returns what
// was saved. Two things differ from SaveSettings: a null value unsets the key
// (its default applies), and SecretMask for a secret setting keeps the stored
// value, so a form that shows masked secrets can be sent back unchanged.
// Keys left out of incoming are unset.
func UpdateSettings(m Manifest, incoming map[string]any) (map[string]any, error) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	root, _, file, err := settingsLocation(m)
	if err != nil {
		return nil, err
	}
	stored, err := readSettings(root, file)
	root.Close()
	if err != nil {
		return nil, err
	}
	next := make(map[string]any, len(incoming))
	for k, v := range incoming {
		if v == nil {
			continue
		}
		if s, ok := m.setting(k); ok && s.Secret && v == SecretMask {
			if old, ok := stored[k]; ok {
				next[k] = old
			}
			continue
		}
		next[k] = normalizeNumber(v)
	}
	if err := saveSettingsLocked(m, next); err != nil {
		return nil, err
	}
	return next, nil
}

func saveSettingsLocked(m Manifest, values map[string]any) error {
	if err := ValidateSettings(m, values); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // "<" stays one byte, not six
	enc.SetIndent("", "  ")
	if err := enc.Encode(values); err != nil {
		return err
	}
	b := buf.Bytes()
	// Each value is within its own cap, but together they can exceed what
	// LoadSettings reads; such a file could never be read or repaired.
	if len(b) > maxSettingsFileBytes {
		return fmt.Errorf("%w: %d bytes encoded, at most %d", ErrSettingsTooLarge, len(b), maxSettingsFileBytes)
	}
	root, dataDir, file, err := settingsLocation(m)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("plugin: creating data directory: %w", err)
	}
	return writeFileAtomic(root, dataDir, file, b)
}

// writeFileAtomic writes b to a new 0600 file next to file and renames it
// over file.
func writeFileAtomic(root *os.Root, dir, file string, b []byte) error {
	rnd := make([]byte, 8)
	if _, err := rand.Read(rnd); err != nil {
		return err
	}
	tmp := path.Join(dir, ".settings-"+hex.EncodeToString(rnd)+".tmp")
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = root.Rename(tmp, file)
	}
	if werr != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("plugin: writing %s: %w", SettingsFile, werr)
	}
	return nil
}
