package external

import (
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
//     fingerprint (it was updated and re-trusted), or whose grants in
//     plugins.grants changed, is replaced: the old process is stopped and the
//     new version starts on first use. The grants are fixed when the process's
//     host handler is built, so a narrower grant must not wait for a restart.
//   - A registered plugin that is now disabled, untrusted, no longer approved
//     for what is on disk (its files changed without a new approval), or gone
//     from the directory is unregistered and its process stopped. A disabled
//     plugin already gets no events (Emit checks the policy), but a live
//     process could still call the host API over its connection, so it is
//     not left running.
//
// If the plugins directory can't be read, a registered plugin is kept only
// while it is still enabled and its approval in the trust store is the one it
// was loaded under; nothing new is registered. If the trust store can't be
// read, every external plugin is treated as untrusted, as at startup.
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
		if cur != nil && cur.approval == approval && slices.Equal(cur.grants, grants) && registered(cur.listener) {
			continue
		}

		client := NewClient(m, plugin.HostHandler(api, grants))
		next := &loaded{client: client, listener: client.EventListener(), approval: approval, grants: grants}
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
		if err != nil && plugin.Enabled(name) && trust != nil && trust.Approved[name] == cur.approval {
			continue // directory unreadable; nothing says this one changed
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
