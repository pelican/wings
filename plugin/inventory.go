package plugin

import (
	"path/filepath"

	"github.com/pelican/wings/config"
)

// Kind distinguishes an in-process native provider from an external one
// described by a manifest.
type Kind string

const (
	KindNative   Kind = "native"
	KindExternal Kind = "external"
)

// ProviderInfo is a read-only description of a provider for operator tooling
// (the `wings plugins` command). It is produced without executing any plugin.
type ProviderInfo struct {
	Name       string     `json:"name"`
	Kind       Kind       `json:"kind"`
	Capability Capability `json:"capability"`
	Version    string     `json:"version,omitempty"`
	Author     string     `json:"author,omitempty"`
	Source     string     `json:"source,omitempty"`
	Update     string     `json:"update,omitempty"`
	Events     []string   `json:"events,omitempty"`
	Transport  string     `json:"transport"`
	Enabled    bool       `json:"enabled"`
	Trusted    bool       `json:"trusted"`
	Reason     string     `json:"reason"`
	Manifest   string     `json:"manifest,omitempty"`
	Err        string     `json:"error,omitempty"`
}

// Inventory returns the combined view of native (registered) and external
// (discovered) providers with their resolved enable state. External providers
// come from scanning the configured plugins directory for manifests; nothing is
// executed. A non-nil error indicates discovery was incomplete (e.g. a bad
// manifest), but any providers gathered before the error are still returned.
func Inventory() ([]ProviderInfo, error) {
	var out []ProviderInfo

	for _, s := range defaultRegistry.snapshot() {
		enabled, reason := enableState(s.Name())
		out = append(out, ProviderInfo{
			Name:       s.Name(),
			Kind:       KindNative,
			Capability: CapabilityEvents,
			Events:     s.Events(),
			Transport:  "in-proc",
			Enabled:    enabled,
			Trusted:    true, // native providers are compiled into Wings
			Reason:     reason,
		})
	}

	// Native providers are always trusted; external ones are gated on operator
	// approval recorded in the trust store.
	trust, trustErr := LoadTrustStore()

	manifests, issues, err := DiscoverDetailed(config.Get().Plugins.Directory)
	for _, issue := range issues {
		name := issueName(issue)
		enabled, reason := enableState(name)
		out = append(out, ProviderInfo{
			Name:      name,
			Kind:      KindExternal,
			Transport: "stdio",
			Enabled:   enabled,
			Trusted:   false,
			Reason:    reason,
			Manifest:  issue.Manifest,
			Err:       issue.Err.Error(),
		})
	}
	for _, m := range manifests {
		enabled, reason := enableState(m.Name)
		trusted := trust != nil && trust.IsTrusted(m)
		errText := ""
		if trustErr != nil {
			trusted = false
			errText = "trust store: " + trustErr.Error()
		}
		out = append(out, ProviderInfo{
			Name:       m.Name,
			Kind:       KindExternal,
			Capability: m.Capability,
			Version:    m.Version,
			Author:     m.Author,
			Source:     m.Source,
			Update:     m.Update,
			Events:     EffectiveEvents(m),
			Transport:  "stdio",
			Enabled:    enabled,
			Trusted:    trusted,
			Reason:     reason,
			Manifest:   filepath.Join(m.Dir(), ManifestFile),
			Err:        errText,
		})
	}

	return out, err
}

func issueName(issue DiscoveryIssue) string {
	if issue.Name != "" {
		return issue.Name
	}
	if issue.Dir != "" {
		return filepath.Base(issue.Dir)
	}
	return filepath.Base(filepath.Dir(issue.Manifest))
}

// Find returns the ProviderInfo for a given name, if present in the inventory.
func Find(name string) (ProviderInfo, bool, error) {
	inv, err := Inventory()
	for _, p := range inv {
		if p.Name == name {
			return p, true, err
		}
	}
	return ProviderInfo{}, false, err
}
