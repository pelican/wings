package install

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/apex/log"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// ErrNotApproved means the staged plugin was not approved, so nothing was
// installed.
var ErrNotApproved = errors.New("not approved")

// Approver is shown the staged manifest and decides whether to install it:
// the CLI asks the operator, the Panel API checks an approval token.
type Approver func(plugin.Manifest) (bool, error)

// Install takes a staged add or update the rest of the way: check, if set,
// vets the staged manifest; approve decides; then the plugin is moved into
// place and the approval is recorded in the trust store. The staging directory
// is removed on every return. Once the plugin is in place, a failure to record
// the approval rolls it back: a plugin left installed without one would never
// load, and for an update the old version would be gone.
//
// Commit takes the plugins directory's install lock, and the trust store takes
// its own, so the CLI and the daemon can install at the same time.
func Install(stage *StagedInstall, check func(plugin.Manifest) error, approve Approver) (plugin.Manifest, error) {
	defer stage.Cleanup()
	m, err := stage.Manifest()
	if err != nil {
		return m, fmt.Errorf("could not read the staged manifest: %w", err)
	}
	if check != nil {
		if err := check(m); err != nil {
			return m, err
		}
	}
	ok, err := approve(m)
	if err != nil {
		return m, err
	}
	if !ok {
		return m, ErrNotApproved
	}
	if err := stage.Commit(); err != nil {
		return m, err
	}
	m, err = plugin.LoadManifest(filepath.Join(stage.DestDir, plugin.ManifestFile))
	if err != nil {
		return m, rollBack(stage, fmt.Errorf("could not read the installed manifest for trust approval: %w", err))
	}
	if err := RecordTrust(m); err != nil {
		return m, rollBack(stage, fmt.Errorf("recording trust: %w", err))
	}
	if err := stage.Finish(); err != nil {
		log.WithField("plugin", m.Name).WithField("error", err).Warn("could not remove the previous version's backup")
	}
	return m, nil
}

// rollBack undoes a committed install after err, and says in the returned
// error whether that worked, since the plugins directory otherwise needs a look.
func rollBack(stage *StagedInstall, err error) error {
	if rbErr := stage.Rollback(); rbErr != nil {
		return fmt.Errorf("%w; could not restore the previous state: %v", err, rbErr)
	}
	return fmt.Errorf("%w; the previous state was restored", err)
}

// RecordTrust approves m, as it is on disk now, in the trust store.
func RecordTrust(m plugin.Manifest) error {
	ts, err := plugin.LoadTrustStore()
	if err != nil {
		return fmt.Errorf("trust store error: %w", err)
	}
	return ts.Approve(m)
}

// Toggle changes plugin policy so name ends up enabled or disabled. The CLI's
// enable/disable and the Panel API both use it, inside
// config.UpdatePluginsOnDisk.
//
// Enabling removes it from the blocklist and, when the allowlist is in force
// (non-empty, or autoload is off so only listed plugins run), adds it there.
// Disabling only adds it to the blocklist, which always wins; it leaves the
// allowlist alone, because removing the last entry would empty the allowlist
// and, with autoload on, turn every other plugin on.
func Toggle(p *config.PluginConfiguration, name string, enable bool) {
	if enable {
		p.Disabled = removeString(p.Disabled, name)
		if (!p.Autoload || len(p.Enabled) > 0) && !containsString(p.Enabled, name) {
			p.Enabled = append(p.Enabled, name)
		}
		return
	}
	if !containsString(p.Disabled, name) {
		p.Disabled = append(p.Disabled, name)
	}
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func removeString(s []string, v string) []string {
	out := s[:0:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
