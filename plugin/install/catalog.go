package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/apex/log"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
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
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Capability  plugin.Capability `json:"capability"`
	Author      string            `json:"author"`
	Description string            `json:"description"`
	MinWings    string            `json:"minWings"`
	Download    string            `json:"download"`            // https URL to the plugin archive
	SHA256      string            `json:"sha256"`              // hex digest of the archive
	Signature   string            `json:"signature,omitempty"` // base64 Ed25519 signature over the archive sha256
	// Target is which side installs the entry: "wings", or "panel" for a Panel
	// plugin listed in the same registry. Missing means "wings".
	Target string `json:"target,omitempty"`

	// badSignature is set when the entry has a "signature" field that is not
	// a non-empty string. Such an entry is invalid, never unsigned.
	badSignature bool
}

// UnmarshalJSON reads an entry, noting a "signature" that is present but not
// a non-empty string ("", null, a number) instead of failing the whole
// registry or reading it as unsigned.
func (e *CatalogEntry) UnmarshalJSON(b []byte) error {
	type plain CatalogEntry
	var raw struct {
		plain
		Signature json.RawMessage `json:"signature"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*e = CatalogEntry(raw.plain)
	e.Signature, e.badSignature = "", false
	if raw.Signature != nil {
		if err := json.Unmarshal(raw.Signature, &e.Signature); err != nil || e.Signature == "" {
			e.Signature, e.badSignature = "", true
		}
	}
	return nil
}

// forWings reports whether Wings installs e: its target is missing or
// "wings". Entries for the Panel, or a target this Wings doesn't know, are
// someone else's.
func (e CatalogEntry) forWings() bool { return e.Target == "" || e.Target == "wings" }

// problem says why e can't be installed even though Wings is its target.
func (e CatalogEntry) problem() error {
	switch {
	case plugin.InvalidName(e.Name):
		return fmt.Errorf("plugin: registry entry has an invalid plugin name %q", e.Name)
	case e.badSignature:
		return fmt.Errorf("plugin: registry entry %q has a signature field that is not a non-empty string; the entry is invalid", e.Name)
	}
	return nil
}

// fetchCatalog downloads and parses a registry index from an https URL, sending
// any private-registry auth.
func fetchCatalog(ctx context.Context, url string, a auth) (Catalog, error) {
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
		if !e.forWings() {
			continue // a Panel plugin, or a target this Wings doesn't know
		}
		if e.Name == name {
			return e, true
		}
	}
	return CatalogEntry{}, false
}

// RegistryListing is what one configured registry lists for Wings.
type RegistryListing struct {
	Namespace string
	// Entries are the registry's entries Wings can install: target "wings"
	// (or none), a valid name and a well-formed signature field.
	Entries []CatalogEntry
	// Err is set, and Entries empty, when the registry could not be read.
	Err error
}

// ListRegistries reads every registry in plugins.registries, each with its
// own credentials, at the same time, and returns them sorted by namespace.
func ListRegistries(ctx context.Context) []RegistryListing {
	regs := registries()
	out := make([]RegistryListing, 0, len(regs))
	for ns := range regs {
		out = append(out, RegistryListing{Namespace: ns})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(l *RegistryListing) {
			defer wg.Done()
			reg := regs[l.Namespace]
			cat, err := fetchCatalog(ctx, reg.URL, authFor(reg))
			if err != nil {
				l.Err = redactURL(err, reg.URL)
				return
			}
			l.Entries = []CatalogEntry{}
			for _, e := range cat.Plugins {
				if !e.forWings() {
					continue
				}
				if err := e.problem(); err != nil {
					log.WithField("registry", l.Namespace).WithField("error", err).Warn("skipping an invalid registry entry")
					continue
				}
				l.Entries = append(l.Entries, e)
			}
		}(&out[i])
	}
	wg.Wait()
	return out
}

// ResolvedPlugin is a registry entry together with the (opaque) credentials
// needed to download it. Obtain one from Resolve and pass it to StageAdd.
type ResolvedPlugin struct {
	Entry CatalogEntry
	auth  auth
	// registryHost is the host the registry index was fetched from. The
	// registry's credentials are only sent to that host.
	registryHost string
}

// downloadAuth returns the credentials to send with the archive download:
// the registry's, but only when the archive is served from the registry's
// own host. An archive on a CDN or object store never sees the token or
// custom headers.
func (r ResolvedPlugin) downloadAuth() auth {
	u, err := url.Parse(r.Entry.Download)
	if err != nil || r.registryHost == "" || u.Host != r.registryHost {
		return auth{}
	}
	return r.auth
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// Resolve turns a reference into a ResolvedPlugin. Accepted forms:
//
//	@namespace/name   resolve the namespace to a registry (with its auth) from config
//	url#name          fetch that registry URL and pick the named entry (no auth)
//	name              search every configured registry; it must be found in
//	                  exactly one, and every registry must be readable
func Resolve(ctx context.Context, ref string) (ResolvedPlugin, error) {
	switch {
	case strings.Contains(ref, "#"):
		url, name, _ := strings.Cut(ref, "#")
		cat, err := fetchCatalog(ctx, url, auth{})
		if err != nil {
			return ResolvedPlugin{}, err
		}
		if e, ok := cat.find(name); ok {
			if err := e.problem(); err != nil {
				return ResolvedPlugin{}, err
			}
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
		cat, err := fetchCatalog(ctx, reg.URL, a)
		if err != nil {
			// The Panel sees this error (install through the plugin API).
			return ResolvedPlugin{}, redactURL(err, reg.URL)
		}
		if e, ok := cat.find(name); ok {
			if err := e.problem(); err != nil {
				return ResolvedPlugin{}, err
			}
			return ResolvedPlugin{Entry: e, auth: a, registryHost: hostOf(reg.URL)}, nil
		}
		return ResolvedPlugin{}, fmt.Errorf("plugin %q not found in registry %q", name, ns)

	default:
		// A bare name must be unambiguous. If two registries list it (say a
		// company's private one and a public one) the operator has to say which
		// with @namespace/name; picking one would let whichever registry
		// published the name first decide what code gets installed. For the
		// same reason a registry that cannot be read fails the lookup instead
		// of being skipped, since it might hold the plugin the operator meant.
		regs := registries()
		namespaces := make([]string, 0, len(regs))
		for ns := range regs {
			namespaces = append(namespaces, ns)
		}
		sort.Strings(namespaces)

		var found []ResolvedPlugin
		var where []string
		for _, ns := range namespaces {
			a := authFor(regs[ns])
			cat, err := fetchCatalog(ctx, regs[ns].URL, a)
			if err != nil {
				return ResolvedPlugin{}, fmt.Errorf("plugin: could not read registry %q (%v); install with @namespace/name to use a specific registry", ns, redactURL(err, regs[ns].URL))
			}
			if e, ok := cat.find(ref); ok {
				found = append(found, ResolvedPlugin{Entry: e, auth: a, registryHost: hostOf(regs[ns].URL)})
				where = append(where, "@"+ns+"/"+ref)
			}
		}
		switch len(found) {
		case 0:
			return ResolvedPlugin{}, fmt.Errorf("plugin %q not found in any configured registry", ref)
		case 1:
			if err := found[0].Entry.problem(); err != nil {
				return ResolvedPlugin{}, err
			}
			return found[0], nil
		default:
			return ResolvedPlugin{}, fmt.Errorf("plugin %q is listed in more than one registry; choose one of: %s", ref, strings.Join(where, ", "))
		}
	}
}

// redactURL returns err with rawURL, wherever it appears, cut down to its
// scheme, host and path: a registry URL's user info or query may carry a
// credential, and these errors are shown to the Panel.
func redactURL(err error, rawURL string) error {
	u, perr := url.Parse(rawURL)
	if perr != nil || (u.User == nil && u.RawQuery == "" && u.Fragment == "") {
		return err
	}
	safe := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	msg := strings.ReplaceAll(err.Error(), rawURL, safe.String())
	if u.RawQuery != "" {
		msg = strings.ReplaceAll(msg, u.RawQuery, "…")
	}
	return errors.New(msg)
}

func registries() map[string]config.PluginRegistry { return config.Get().Plugins.Registries }

// StageAdd downloads and extracts a registry plugin without committing it. The
// staged manifest must match the registry entry name.
func StageAdd(ctx context.Context, r ResolvedPlugin, pluginsDir string, opts ...StageOption) (*StagedInstall, error) {
	e := r.Entry
	if e.Download == "" || e.SHA256 == "" {
		return nil, fmt.Errorf("plugin %q: registry entry is missing download or sha256", e.Name)
	}
	// Guard the name so it cannot escape the plugins directory.
	if plugin.InvalidName(e.Name) {
		return nil, fmt.Errorf("plugin: invalid plugin name %q", e.Name)
	}
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return nil, fmt.Errorf("plugin: creating plugins directory: %w", err)
	}
	dest := filepath.Join(pluginsDir, e.Name)
	stage, err := stageArchive(ctx, e.Download, e.SHA256, e.Signature, dest, r.downloadAuth(), applyStageOptions(opts))
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
