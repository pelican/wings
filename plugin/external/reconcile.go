package external

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/apex/log"

	"github.com/pelican/wings/plugin"
)

// loaded is an external plugin this package registered.
type loaded struct {
	client   *Client
	listener plugin.EventListener
	approval string   // the trust store fingerprint it was loaded under
	grants   []string // the host methods it was given
	dir      string   // the plugin directory it runs in
}

// stillValid reports whether l may keep running as it is, judged without the
// plugins directory's listing: it is enabled, its files on disk still match
// the approval it was loaded under, and its host grants are the ones it was
// given.
func (l *loaded) stillValid(trust *plugin.TrustStore) bool {
	m := l.client.manifest
	return plugin.Enabled(m.Name) && trust != nil && trust.Approved[m.Name] == l.approval &&
		trust.IsTrusted(m) && slices.Equal(plugin.GrantsFor(m), l.grants)
}

// verifyApproval returns the check a client runs before it starts the plugin
// process: the files on disk must still be the ones approved as approval. The
// watcher stops a changed plugin within a poll or two; this keeps it from
// being started (or restarted after a crash) in between.
func verifyApproval(m plugin.Manifest, approval string) func() error {
	return func() error {
		ts, err := plugin.LoadTrustStore()
		if err != nil {
			return fmt.Errorf("reading the trust store: %w", err)
		}
		if ts.Approved[m.Name] != approval || !ts.IsTrusted(m) {
			return errors.New("its files or approval changed since it was loaded; run `wings plugins trust " + m.Name + "` to approve them")
		}
		return nil
	}
}

var (
	reconcileMu sync.Mutex
	running     = map[string]*loaded{} // by plugin name; guarded by reconcileMu
)

// Reconcile makes the registered external plugins match the plugins directory,
// the trust store and the enable policy as they are now. It is safe to call
// at any time and from several goroutines; the daemon calls it at startup and
// after every policy change (plugin.AfterPolicyChange).
//
//   - An enabled, trusted plugin that is not registered is registered. Its
//     process starts on first use, as at startup.
//   - A registered plugin whose approval fingerprint and host grants are
//     unchanged is left alone: its process keeps running.
//   - A registered plugin that was approved again with a different
//     fingerprint (it was updated and re-trusted), whose grants in
//     plugins.grants changed, or whose directory moved, is replaced: the old
//     process is stopped and the new version starts on first use. The grants
//     are fixed when the process's host handler is built, so a narrower grant
//     must not wait for a restart.
//   - A registered plugin that is now disabled, untrusted, no longer approved
//     for what is on disk (its files changed without a new approval), or gone
//     from the directory is unregistered and its process stopped. A disabled
//     plugin already gets no events (Emit checks the policy), but a live
//     process could still call the host API over its connection, so it is
//     not left running.
//
// If the plugins directory can't be listed, a registered plugin is kept only
// while it is still enabled, its approval in the trust store is the one it
// was loaded under, its files still match that approval, and its grants are
// unchanged; anything that can't be checked stops it. Nothing new is
// registered. If the trust store can't be read, every external plugin is
// treated as untrusted, as at startup.
func Reconcile(dir string, api plugin.HostAPI) error {
	reconcileMu.Lock()
	defer reconcileMu.Unlock()

	manifests, issues, err := plugin.DiscoverDetailed(dir)
	for _, issue := range issues {
		log.WithField("manifest", issue.Manifest).
			WithField("error", issue.Err).
			Warn("external plugin discovery issue")
	}

	trust, terr := plugin.LoadTrustStore()
	if terr != nil {
		log.WithField("error", terr).Warn("could not load plugin trust store; treating external plugins as untrusted")
	}

	keep := make(map[string]bool, len(manifests))
	for _, m := range manifests {
		if !plugin.Enabled(m.Name) {
			continue
		}
		// Workspace-Trust gate: an external plugin only loads once the operator
		// has explicitly approved its security-relevant manifest fields.
		if trust == nil || !trust.IsTrusted(m) {
			log.WithField("plugin", m.Name).
				Warn("external plugin is not trusted; run `wings plugins trust` to approve — skipping")
			continue
		}
		if m.Capability != plugin.CapabilityEvents {
			log.WithField("plugin", m.Name).
				WithField("capability", string(m.Capability)).
				Warn("unknown plugin capability; skipping")
			continue
		}
		keep[m.Name] = true
		approval := trust.Approved[m.Name]
		grants := plugin.GrantsFor(m)
		cur := running[m.Name]
		if cur != nil && cur.approval == approval && slices.Equal(cur.grants, grants) && cur.dir == m.Dir() && registered(cur.listener) {
			continue
		}

		client := NewClient(m, plugin.HostHandler(api, grants))
		client.verify = verifyApproval(m, approval)
		next := &loaded{client: client, listener: client.EventListener(), approval: approval, grants: grants, dir: m.Dir()}
		// Registering under the same name replaces the old listener, so no
		// event reaches the old process after this.
		plugin.RegisterEventListener(next.listener)
		running[m.Name] = next
		l := log.WithField("plugin", m.Name).WithField("version", m.Version)
		if cur != nil {
			_ = cur.client.Close()
			l.Info("reloaded external plugin: its approval or grants changed")
		} else {
			l.Info("registered external plugin")
		}
	}

	for name, cur := range running {
		if keep[name] {
			continue
		}
		if err != nil && cur.stillValid(trust) {
			continue // directory unreadable, but this one is checked unchanged
		}
		plugin.Default().RemoveEventListener(cur.listener)
		_ = cur.client.Close()
		delete(running, name)
		log.WithField("plugin", name).Info("stopped external plugin: it is disabled, untrusted, changed or removed")
	}
	return err
}

// registered reports whether l is still the listener registered under its
// name: something else may have registered under the same name since.
func registered(l plugin.EventListener) bool {
	got, ok := plugin.Default().EventListener(l.Name())
	return ok && got == l
}
