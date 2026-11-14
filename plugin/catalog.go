package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelican/wings/config"
)

const maxCatalogBytes = 1 << 20 // 1 MiB registry index

// Catalog is a plugin registry index — the JSON a registry publishes (à la a
// shadcn registry.json). It is a plain static file served over HTTPS; there is
// no central server.
type Catalog struct {
	Name    string         `json:"name"`
	Plugins []CatalogEntry `json:"plugins"`
}

// CatalogEntry describes one installable plugin in a registry.
type CatalogEntry struct {
	Name        string     `json:"name"`
	Version     string     `json:"version"`
	Capability  Capability `json:"capability"`
	Author      string     `json:"author"`
	Description string     `json:"description"`
	MinWings    string     `json:"minWings"`
	Download    string     `json:"download"`  // https URL to the plugin archive
	SHA256      string     `json:"sha256"`    // hex digest of the archive
	Signature   string     `json:"signature"` // base64 Ed25519 signature over the archive sha256
}

// FetchCatalog downloads and parses a registry index from an https URL, sending
// any private-registry auth.
func FetchCatalog(ctx context.Context, url string, a auth) (Catalog, error) {
	body, err := httpsGet(ctx, url, maxCatalogBytes, a)
	if err != nil {
		return Catalog{}, err
	}
	defer body.Close()

	var c Catalog
	if err := json.NewDecoder(body).Decode(&c); err != nil {
		return Catalog{}, fmt.Errorf("plugin: decoding registry index: %w", err)
	}
	return c, nil
}

func (c Catalog) find(name string) (CatalogEntry, bool) {
	for _, e := range c.Plugins {
		if e.Name == name {
			return e, true
		}
	}
	return CatalogEntry{}, false
}

// ResolvedPlugin is a registry entry together with the (opaque) credentials
// needed to download it. Obtain one from Resolve and pass it to Add.
type ResolvedPlugin struct {
	Entry CatalogEntry
	auth  auth
}

// Resolve turns a reference into a ResolvedPlugin. Accepted forms:
//
//	@namespace/name   resolve the namespace to a registry (with its auth) from config
//	url#name          fetch that registry URL and pick the named entry (no auth)
//	name              search every configured registry, first match wins
func Resolve(ctx context.Context, ref string) (ResolvedPlugin, error) {
	switch {
	case strings.Contains(ref, "#"):
		url, name, _ := strings.Cut(ref, "#")
		cat, err := FetchCatalog(ctx, url, auth{})
		if err != nil {
			return ResolvedPlugin{}, err
		}
		if e, ok := cat.find(name); ok {
			return ResolvedPlugin{Entry: e}, nil
		}
		return ResolvedPlugin{}, fmt.Errorf("plugin %q not found in %s", name, url)

	case strings.HasPrefix(ref, "@"):
		ns, name, ok := strings.Cut(strings.TrimPrefix(ref, "@"), "/")
		if !ok || ns == "" || name == "" {
			return ResolvedPlugin{}, fmt.Errorf("plugin: invalid reference %q, expected @namespace/name", ref)
		}
		reg, ok := registries()[ns]
		if !ok {
			return ResolvedPlugin{}, fmt.Errorf("plugin: unknown registry namespace %q; configure it under plugins.registries", ns)
		}
		a := authFor(reg)
		cat, err := FetchCatalog(ctx, reg.URL, a)
		if err != nil {
			return ResolvedPlugin{}, err
		}
		if e, ok := cat.find(name); ok {
			return ResolvedPlugin{Entry: e, auth: a}, nil
		}
		return ResolvedPlugin{}, fmt.Errorf("plugin %q not found in registry %q", name, ns)

	default:
		for _, reg := range registries() {
			a := authFor(reg)
			cat, err := FetchCatalog(ctx, reg.URL, a)
			if err != nil {
				continue // skip an unreachable registry, try the rest
			}
			if e, ok := cat.find(ref); ok {
				return ResolvedPlugin{Entry: e, auth: a}, nil
			}
		}
		return ResolvedPlugin{}, fmt.Errorf("plugin %q not found in any configured registry", ref)
	}
}

func registries() map[string]config.PluginRegistry { return config.Get().Plugins.Registries }

// Add installs a resolved plugin into pluginsDir, reusing the same verified
// download + signature check + extract + atomic swap the updater uses. The
// running Wings must be restarted to load the new plugin.
func Add(ctx context.Context, r ResolvedPlugin, pluginsDir string) error {
	stage, err := StageAdd(ctx, r, pluginsDir)
	if err != nil {
		return err
	}
	defer stage.Cleanup()
	return stage.Commit()
}

// StageAdd downloads and extracts a registry plugin without committing it. The
// staged manifest must match the registry entry name.
func StageAdd(ctx context.Context, r ResolvedPlugin, pluginsDir string) (*StagedInstall, error) {
	e := r.Entry
	if e.Download == "" || e.SHA256 == "" {
		return nil, fmt.Errorf("plugin %q: registry entry is missing download or sha256", e.Name)
	}
	// Guard the name so it cannot escape the plugins directory.
	if invalidPluginName(e.Name) {
		return nil, fmt.Errorf("plugin: invalid plugin name %q", e.Name)
	}
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return nil, fmt.Errorf("plugin: creating plugins directory: %w", err)
	}
	dest := filepath.Join(pluginsDir, e.Name)
	stage, err := stageArchive(ctx, e.Download, e.SHA256, e.Signature, dest, r.auth)
	if err != nil {
		return nil, err
	}
	m, err := stage.Manifest()
	if err != nil {
		_ = stage.Cleanup()
		return nil, err
	}
	if m.Name != e.Name {
		_ = stage.Cleanup()
		return nil, fmt.Errorf("plugin: archive manifest name %q does not match registry entry %q", m.Name, e.Name)
	}
	if e.Version != "" && m.Version != e.Version {
		_ = stage.Cleanup()
		return nil, fmt.Errorf("plugin: archive manifest version %q does not match registry entry %q", m.Version, e.Version)
	}
	if e.Capability != "" && m.Capability != e.Capability {
		_ = stage.Cleanup()
		return nil, fmt.Errorf("plugin: archive manifest capability %q does not match registry entry %q", m.Capability, e.Capability)
	}
	return stage, nil
}
