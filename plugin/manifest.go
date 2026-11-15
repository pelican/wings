package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManifestFile is the filename an external plugin provides to describe itself.
const ManifestFile = "plugin.yaml"

// Manifest is the static, execution-free description an external plugin ships in
// its directory. It is read during discovery so that every trust decision —
// whether to run the plugin at all, and which host capabilities to grant it —
// can be made BEFORE the plugin process is ever spawned. This is the "discover
// != execute" rule that keeps drop-in registration safe.
type Manifest struct {
	APIVersion string     `yaml:"apiVersion"`
	Name       string     `yaml:"name"`
	Capability Capability `yaml:"capability"`

	// Authoring metadata. Optional for a plugin to run, but expected for anything
	// distributed; a marketplace (M5) would require them.
	Version     string `yaml:"version"`
	Author      string `yaml:"author"`
	Description string `yaml:"description"`
	License     string `yaml:"license"`
	// Source is the plugin's repository (e.g. a GitHub URL) — where users find
	// the code, issues, and releases.
	Source string `yaml:"source"`
	// Update is a URL to an update manifest ({version, download, sha256,
	// signature}) that a future auto-updater (M5) checks to offer upgrades.
	Update string `yaml:"update"`

	// MinWings is the minimum Wings version this plugin supports.
	MinWings   string   `yaml:"minWings"`
	Entrypoint []string `yaml:"entrypoint"`
	Provides   Provides `yaml:"provides"`
	Requires   Requires `yaml:"requires"`

	// dir is the absolute directory the manifest was loaded from; entrypoint
	// paths are resolved relative to it.
	dir string
}

// Provides declares what a plugin offers.
type Provides struct {
	// Events lists the event types an events-capability plugin subscribes to
	// ("*" = all).
	Events []string `yaml:"events"`
}

// Requires declares the host capabilities a plugin asks for (the grant request).
type Requires struct {
	HostAPI []string `yaml:"hostAPI"`
}

// Dir returns the directory the manifest was loaded from.
func (m Manifest) Dir() string { return m.dir }

// DataDirName is the hidden directory in the plugins directory that holds each
// plugin's own settings and state, one sub directory per plugin name.
const DataDirName = ".data"

// DataDir is where this plugin keeps its settings and state:
// <plugins directory>/.data/<name>. It is outside the plugin's own directory
// on purpose: updates replace that directory and the trust approval hashes it,
// so settings kept there would be deleted by an update and would invalidate the
// approval whenever they were edited. Empty when the manifest was not loaded
// from disk.
func (m Manifest) DataDir() string {
	if m.dir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(m.dir), DataDirName, m.Name)
}

// EffectiveEvents returns the event scope an events plugin registers for. An
// empty list preserves the historical default of receiving all events.
func EffectiveEvents(m Manifest) []string {
	if len(m.Provides.Events) == 0 && m.Capability == CapabilityEvents {
		return []string{"*"}
	}
	return m.Provides.Events
}

// LoadManifest reads and validates a single manifest file. It never executes
// anything referenced by the manifest.
func LoadManifest(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin: reading manifest %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("plugin: parsing manifest %s: %w", path, err)
	}
	m.dir = filepath.Dir(path)
	if err := m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) validate() error {
	switch {
	case m.APIVersion == "":
		return fmt.Errorf("plugin: manifest in %s missing apiVersion", m.dir)
	case m.Name == "":
		return fmt.Errorf("plugin: manifest in %s missing name", m.dir)
	case InvalidName(m.Name):
		return fmt.Errorf("plugin: manifest in %s has invalid name %q", m.dir, m.Name)
	case m.Capability == "":
		return fmt.Errorf("plugin: manifest for %q missing capability", m.Name)
	case len(m.Entrypoint) == 0:
		return fmt.Errorf("plugin: manifest for %q missing entrypoint", m.Name)
	}
	return nil
}

// InvalidName reports whether name cannot be used as a plugin's own
// directory under the plugins directory. Separators and ".." are refused, and
// so is a leading ".": filepath.Join(dir, ".") is dir itself, so installing a
// plugin named "." would replace every plugin and the trust store, and hidden
// directories are where installs keep their staging and backup copies, which
// discovery skips.
func InvalidName(name string) bool {
	return name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..")
}

// DiscoveryIssue is a per-plugin discovery failure. It lets operator tooling
// show every bad manifest instead of losing later valid plugins after the first
// error.
type DiscoveryIssue struct {
	Name     string
	Dir      string
	Manifest string
	Err      error
}

func (i DiscoveryIssue) Error() string {
	name := i.Name
	if name == "" {
		name = filepath.Base(i.Dir)
	}
	if name == "." || name == "" {
		name = i.Manifest
	}
	return fmt.Sprintf("%s: %v", name, i.Err)
}

// DiscoveryError aggregates per-plugin discovery failures returned by Discover.
type DiscoveryError []DiscoveryIssue

func (e DiscoveryError) Error() string {
	if len(e) == 1 {
		return e[0].Error()
	}
	var parts []string
	for _, issue := range e {
		parts = append(parts, issue.Error())
	}
	return fmt.Sprintf("plugin: %d discovery issues: %s", len(e), strings.Join(parts, "; "))
}

// Discover scans dir for immediate sub-directories containing a manifest file
// and returns every manifest that loads and validates. Directories without a
// manifest are skipped, and a missing plugins directory is not an error. No
// plugin is executed: this is the read-only half of "discover != execute".
// Callers apply enable/grant policy to the result before anything is spawned
// (external-transport wiring lands in a later milestone).
func Discover(dir string) ([]Manifest, error) {
	manifests, issues, err := DiscoverDetailed(dir)
	if err != nil {
		return nil, err
	}
	if len(issues) > 0 {
		return manifests, DiscoveryError(issues)
	}
	return manifests, nil
}

// DiscoverDetailed is the reporting form of Discover. It keeps scanning after a
// bad manifest and rejects duplicate external plugin names by omitting all
// colliding manifests from the returned set.
func DiscoverDetailed(dir string) ([]Manifest, []DiscoveryIssue, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("plugin: scanning plugin directory %s: %w", dir, err)
	}
	var loaded []Manifest
	var issues []DiscoveryIssue
	for _, e := range entries {
		// Hidden directories hold in-progress installs and the previous
		// version during an update (.wings-install-*, .wings-backup-*). One
		// left behind by an interrupted run would otherwise show up as a
		// second copy of the plugin and get both copies rejected.
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name(), ManifestFile)
		if _, err := os.Stat(path); err != nil {
			continue // no manifest in this directory; skip
		}
		m, err := LoadManifest(path)
		if err != nil {
			issues = append(issues, DiscoveryIssue{
				Dir:      filepath.Join(dir, e.Name()),
				Manifest: path,
				Err:      err,
			})
			continue
		}
		loaded = append(loaded, m)
	}

	counts := make(map[string]int, len(loaded))
	for _, m := range loaded {
		counts[m.Name]++
	}

	out := make([]Manifest, 0, len(loaded))
	for _, m := range loaded {
		if counts[m.Name] > 1 {
			issues = append(issues, DiscoveryIssue{
				Name:     m.Name,
				Dir:      m.Dir(),
				Manifest: filepath.Join(m.Dir(), ManifestFile),
				Err:      fmt.Errorf("plugin: duplicate external plugin name %q", m.Name),
			})
			continue
		}
		out = append(out, m)
	}
	return out, issues, nil
}
