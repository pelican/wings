package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"emperror.dev/errors"
	"github.com/goccy/go-json"

	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/system"
)

// idPattern is what a plugin id may contain. It doubles as the plugin's
// directory name, so it has to be a safe path segment: no separators, no
// leading dot, nothing that could resolve upwards out of the plugin directory.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// SettingType is the kind of input the Panel renders for a declared setting.
type SettingType string

const (
	SettingString  SettingType = "string"
	SettingText    SettingType = "text"
	SettingNumber  SettingType = "number"
	SettingBoolean SettingType = "boolean"
	SettingSelect  SettingType = "select"
	SettingSecret  SettingType = "secret"
)

// Setting is one configuration value a plugin accepts, as recorded in the
// manifest.
//
// A plugin declares its settings in code, on its api.Registration, alongside
// the routes and jobs it registers. Wings writes the declared schema here when
// the plugin loads, which is what lets the Panel render a form for a plugin
// that is currently disabled: the code is the source of truth, and this is the
// remembered copy of it.
//
// Editing this by hand in a plugin.json has no lasting effect, since the next
// load overwrites it with whatever the code declares.
type Setting struct {
	// Key is how the plugin reads the value back through api.Settings.
	Key string `json:"key"`

	// Label is the field name shown to the operator. Falls back to the key.
	Label string `json:"label,omitempty"`

	// Description is help text shown under the field.
	Description string `json:"description,omitempty"`

	Type SettingType `json:"type"`

	// Default is used until the operator saves something else.
	Default any `json:"default,omitempty"`

	// Required marks a setting the Panel will not let the operator leave
	// empty. Wings does not enforce it: a plugin should still cope with a
	// missing value.
	Required bool `json:"required,omitempty"`

	// Options are the choices for a select setting.
	Options []SettingOption `json:"options,omitempty"`
}

// SettingOption is one choice for a select setting.
type SettingOption struct {
	Value any    `json:"value"`
	Label string `json:"label"`
}

// Meta is the mutable part of a manifest: the state Wings itself writes back.
//
// Keeping it inside plugin.json rather than in a separate database means a
// plugin directory is self-describing. Copy the directory to another node and
// it arrives with its settings and its enabled state intact, which is how the
// Panel's own plugins behave.
type Meta struct {
	Status        Status `json:"status"`
	StatusMessage string `json:"status_message,omitempty"`

	// LoadOrder decides the order plugins load in, and therefore the order
	// hooks run in. Lower loads first.
	LoadOrder int `json:"load_order"`

	// Settings are the operator's saved values, keyed by Setting.Key.
	Settings map[string]any `json:"settings,omitempty"`
}

// Manifest is a plugin's plugin.json.
//
// The field names mirror the Panel's plugin manifest wherever the two mean the
// same thing, so an operator reading both sees one format. The differences are
// the ones that have to differ: a Wings plugin names a Go package and
// entrypoint where a Panel plugin names a PHP namespace and class, and there
// are no composer packages because a Wings plugin cannot pull dependencies.
type Manifest struct {
	// ID identifies the plugin and must equal the name of the directory
	// holding it.
	ID string `json:"id"`

	Name        string `json:"name"`
	Author      string `json:"author"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`

	Category Category `json:"category"`

	// URL is the plugin's homepage.
	URL string `json:"url,omitempty"`

	// UpdateURL points at a JSON document describing available versions, in
	// the same format the Panel's plugin updater expects, so one document can
	// serve both halves of a plugin.
	UpdateURL string `json:"update_url,omitempty"`

	// APIVersion is the plugin API contract the plugin was written against.
	// Absent means 1.
	APIVersion int `json:"api_version,omitempty"`

	// WingsVersion constrains which Wings versions will load this plugin.
	// A bare version such as "1.2.3" means exactly that version; a caret
	// range such as "^1.2" means that version or any compatible newer one,
	// with the same semantics the Panel applies to panel_version.
	WingsVersion string `json:"wings_version,omitempty"`

	// Package is the Go package name shared by the plugin's .go files. It is
	// how Wings addresses the entrypoint after interpreting the source.
	Package string `json:"package"`

	// Entrypoint is the function returning the plugin's api.Registration.
	// Absent means "New".
	Entrypoint string `json:"entrypoint,omitempty"`

	// Dependencies are Go modules the plugin imports, as module path to
	// version, in the same spirit as a Panel plugin's composer_packages.
	//
	// Wings fetches each one's source from the configured module proxy and
	// makes it importable. There is no build step, so a module only works if
	// it can be interpreted: anything relying on cgo, assembly, or a part of
	// the standard library plugins are not given will fail to load, and the
	// plugin is refused with the reason rather than half-loaded.
	//
	// Versions must be exact, such as "v1.6.0". A range would mean the same
	// plugin behaving differently on two nodes.
	Dependencies map[string]string `json:"dependencies,omitempty"`

	// DependencyHashes optionally pins the SHA-256 of each module archive,
	// keyed "<module path>@<version>". A declared hash that does not match
	// refuses the plugin; an undeclared one is fetched and its hash logged,
	// so an author can pin it afterwards.
	DependencyHashes map[string]string `json:"dependency_hashes,omitempty"`

	// Settings is the configuration surface the plugin declared the last time
	// it loaded. Written by Wings, not by the plugin author; see [Setting].
	Settings []Setting `json:"settings,omitempty"`

	// Meta is the state Wings writes back: status, load order and saved
	// settings.
	Meta Meta `json:"meta"`

	// dir is where this manifest was read from. Not serialised.
	dir string `json:"-"`
}

// EffectiveAPIVersion is the contract version the plugin targets, defaulting to
// 1 for a manifest that does not say.
func (m *Manifest) EffectiveAPIVersion() int {
	if m.APIVersion <= 0 {
		return 1
	}
	return m.APIVersion
}

// EffectiveEntrypoint is the function Wings calls to build the plugin.
func (m *Manifest) EffectiveEntrypoint() string {
	if m.Entrypoint == "" {
		return "New"
	}
	return m.Entrypoint
}

// Directory is where the plugin's files live.
func (m Manifest) Directory() string { return m.dir }

// APIVersionSupported reports whether this daemon implements a contract version
// the plugin can work against.
func (m *Manifest) APIVersionSupported() bool {
	return m.EffectiveAPIVersion() <= api.Version
}

// WingsVersionStrict reports whether the plugin demands one exact Wings
// version rather than a compatible range.
func (m *Manifest) WingsVersionStrict() bool {
	return m.WingsVersion != "" && !strings.HasPrefix(m.WingsVersion, "^")
}

// Compatible reports whether the running Wings satisfies the manifest's
// wings_version constraint.
//
// A development build reports no comparable version, and rather than refuse
// every plugin on a dev daemon we let them all through: the operator running a
// dev build is the one person equipped to notice.
func (m *Manifest) Compatible() bool {
	if m.WingsVersion == "" {
		return true
	}

	current := comparableVersion(system.Version)
	if current == "" {
		return true
	}

	if m.WingsVersionStrict() {
		return compareVersions(current, normalizeVersion(m.WingsVersion)) == 0
	}

	// "^X.Y.Z" means at least X.Y.Z and below the next major, capping at the
	// next minor for 0.x and the next patch for 0.0.x, matching what the Panel
	// does for panel_version.
	parts := strings.Split(strings.TrimPrefix(m.WingsVersion, "^"), ".")
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	minimum := strings.Join(parts[:3], ".")

	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	patch, _ := strconv.Atoi(parts[2])

	var upper string
	switch {
	case major != 0:
		upper = fmt.Sprintf("%d.0.0", major+1)
	case minor != 0:
		upper = fmt.Sprintf("0.%d.0", minor+1)
	default:
		upper = fmt.Sprintf("0.0.%d", patch+1)
	}

	return compareVersions(current, minimum) >= 0 && compareVersions(current, upper) < 0
}

// Validate checks the parts of a manifest Wings cannot work without, and
// returns every problem at once rather than the first, so a plugin author
// fixing a manifest sees the whole list.
func (m *Manifest) Validate(expectedID string) error {
	var problems []string

	switch {
	case m.ID == "":
		problems = append(problems, "id is required")
	case !idPattern.MatchString(m.ID):
		problems = append(problems, "id must be lowercase and may contain only letters, digits, dots, dashes and underscores")
	case expectedID != "" && m.ID != expectedID:
		problems = append(problems, fmt.Sprintf("id %q does not match the directory name %q", m.ID, expectedID))
	}

	if m.Name == "" {
		problems = append(problems, "name is required")
	}
	if m.Author == "" {
		problems = append(problems, "author is required")
	}
	if m.Version == "" {
		problems = append(problems, "version is required")
	}
	if m.Package == "" {
		problems = append(problems, "package is required and must be the Go package name used by the plugin's .go files")
	}
	if m.Category != "" && !m.Category.Valid() {
		problems = append(problems, fmt.Sprintf("category %q is not one of plugin, backup, parser, integration, security, monitoring", m.Category))
	}

	// Settings are not checked here. They are declared in the plugin's code
	// and validated when it loads, so a manifest carrying a stale cached copy
	// is not a reason to refuse the plugin.

	if len(problems) > 0 {
		return errors.New("plugin.json is invalid: " + strings.Join(problems, "; "))
	}
	return nil
}

// SettingValue returns the operator's saved value for a key, falling back to
// the declared default.
func (m *Manifest) SettingValue(key string) (any, bool) {
	if v, ok := m.Meta.Settings[key]; ok {
		return v, true
	}
	for _, s := range m.Settings {
		if s.Key == key && s.Default != nil {
			return s.Default, true
		}
	}
	return nil, false
}

// ResolvedSettings is every declared setting with its effective value, which is
// what the plugin reads through api.Settings.
func (m *Manifest) ResolvedSettings() map[string]any {
	out := make(map[string]any, len(m.Settings)+len(m.Meta.Settings))
	for _, s := range m.Settings {
		if s.Default != nil {
			out[s.Key] = s.Default
		}
	}
	// Saved values win over declared defaults. Values with no matching
	// declaration are kept too: a plugin that outgrew its manifest should not
	// silently lose an operator's configuration.
	for k, v := range m.Meta.Settings {
		out[k] = v
	}
	return out
}

// LoadManifest reads and validates the plugin.json in dir.
func LoadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, ManifestName)

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrapf(err, "plugins: could not read %s", path)
	}

	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, errors.Wrapf(err, "plugins: %s is not valid JSON", path)
	}

	m.ID = strings.ToLower(strings.TrimSpace(m.ID))
	m.dir = dir

	if err := m.Validate(filepath.Base(dir)); err != nil {
		return nil, err
	}

	if m.Category == "" {
		m.Category = CategoryPlugin
	}
	if !m.Meta.Status.Valid() {
		m.Meta.Status = StatusNotInstalled
	}

	return &m, nil
}

// Save writes the manifest back to disk, preserving any keys Wings does not
// know about.
//
// A plugin author is free to put their own fields in plugin.json, and a Wings
// upgrade may add fields an older daemon drops on the floor. Rather than
// re-serialising our own struct over the top, we merge our fields into the
// document that is already there, so nothing outside our schema is lost when an
// operator flips a plugin on and off.
func (m *Manifest) Save() error {
	path := filepath.Join(m.dir, ManifestName)

	existing := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		// A manifest we cannot parse is one we should not try to merge into;
		// fall through to writing our own fields, which at least leaves a
		// valid file behind.
		_ = json.Unmarshal(b, &existing)
	}

	ours, err := json.Marshal(m)
	if err != nil {
		return errors.Wrap(err, "plugins: could not encode manifest")
	}
	var mine map[string]any
	if err := json.Unmarshal(ours, &mine); err != nil {
		return errors.Wrap(err, "plugins: could not normalise manifest")
	}
	for k, v := range mine {
		existing[k] = v
	}

	b, err := json.MarshalIndent(existing, "", "    ")
	if err != nil {
		return errors.Wrap(err, "plugins: could not encode manifest")
	}
	b = append(b, '\n')

	// Write to a sibling temporary file and rename over the original, so a
	// crash or a full disk cannot leave a plugin with a half-written manifest
	// that stops it loading at all.
	tmp, err := os.CreateTemp(m.dir, ".plugin.json.*")
	if err != nil {
		return errors.Wrap(err, "plugins: could not create temporary manifest")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return errors.Wrap(err, "plugins: could not write temporary manifest")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.Wrap(err, "plugins: could not flush temporary manifest")
	}
	if err := tmp.Close(); err != nil {
		return errors.Wrap(err, "plugins: could not close temporary manifest")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.Wrap(err, "plugins: could not replace manifest")
	}
	return nil
}

// settingsFromRegistration converts what a plugin declared in code into the
// form stored in its manifest.
func settingsFromRegistration(declared []api.Setting) []Setting {
	if len(declared) == 0 {
		return nil
	}

	out := make([]Setting, 0, len(declared))
	for _, d := range declared {
		setting := Setting{
			Key:         d.Key,
			Label:       d.Label,
			Description: d.Description,
			Type:        SettingType(d.Type),
			Default:     d.Default,
			Required:    d.Required,
		}

		// An unset type is a string, which is what an author leaving it out
		// almost always means.
		if setting.Type == "" {
			setting.Type = SettingString
		}

		for _, o := range d.Options {
			setting.Options = append(setting.Options, SettingOption{Value: o.Value, Label: o.Label})
		}

		out = append(out, setting)
	}
	return out
}

// sameSettings reports whether two schemas are equivalent, so a load only
// rewrites the manifest when the declaration actually changed.
func sameSettings(a, b []Setting) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i].Key != b[i].Key ||
			a[i].Label != b[i].Label ||
			a[i].Description != b[i].Description ||
			a[i].Type != b[i].Type ||
			a[i].Required != b[i].Required ||
			fmt.Sprint(a[i].Default) != fmt.Sprint(b[i].Default) ||
			len(a[i].Options) != len(b[i].Options) {
			return false
		}
		for j := range a[i].Options {
			if a[i].Options[j].Label != b[i].Options[j].Label ||
				fmt.Sprint(a[i].Options[j].Value) != fmt.Sprint(b[i].Options[j].Value) {
				return false
			}
		}
	}
	return true
}
