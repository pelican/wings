package plugins

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"gorm.io/gorm"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugins/api"
)

// Manager owns every plugin on the node: finding them on disk, loading the
// enabled ones, and moving them between states when the Panel asks.
//
// It is the only thing that writes a plugin's status, so status in the manifest
// and status in memory cannot drift apart.
type Manager struct {
	// mu guards instances and order. Hook dispatch only reads, so a slow
	// install cannot stall dispatch for long.
	mu sync.RWMutex

	cfg    config.PluginConfiguration
	bridge Bridge
	db     *gorm.DB

	// instances holds every plugin found on disk, loaded or not, keyed by id.
	instances map[string]*Instance

	// order is the ids sorted by load order, then by id to break ties. Hooks
	// run in this order, so it decides which plugin gets to refuse something
	// first.
	order []string

	// scheduler is where plugin jobs go. Set after construction, because
	// Wings builds its scheduler after the manager.
	scheduler JobScheduler

	// routes, backups, parsers and jobs are what loaded plugins have
	// published. They are registries rather than Gin routes or gocron jobs
	// created up front, so enabling and disabling a plugin takes effect
	// without restarting the daemon.
	routes  map[string][]api.Route
	backups map[string]backupRegistration
	parsers map[string]parserRegistration
	jobs    map[string][]string
}

// NewManager builds a manager over the configured plugin directory. It does not
// load anything: call Discover and then LoadEnabled once the rest of the daemon
// is up, so a plugin's Init can already reach servers.
func NewManager(cfg config.PluginConfiguration, bridge Bridge, db *gorm.DB) *Manager {
	return &Manager{
		cfg:       cfg,
		bridge:    bridge,
		db:        db,
		instances: map[string]*Instance{},
	}
}

// Enabled reports whether the plugin subsystem is switched on for this node.
func (m *Manager) Enabled() bool { return m.cfg.Enabled }

// Directory is where the manager looks for plugins.
func (m *Manager) Directory() string { return m.cfg.Directory }

func (m *Manager) log() *log.Entry { return log.WithField("subsystem", "plugins") }

// Discover rescans the plugin directory, adding plugins that have appeared and
// forgetting ones whose directory is gone.
//
// A plugin whose manifest will not parse is not skipped silently: it is kept as
// an errored instance carrying the parse error, so an operator sees the broken
// plugin and the reason in the Panel rather than wondering why it vanished.
func (m *Manager) Discover() error {
	if !m.cfg.Enabled {
		return nil
	}

	entries, err := os.ReadDir(m.cfg.Directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrapf(err, "plugins: could not read plugin directory %s", m.cfg.Directory)
	}

	found := map[string]bool{}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()

		// Skip the dotfile directories install uses while staging an archive,
		// so a plugin being imported is never half-discovered.
		if strings.HasPrefix(name, ".") {
			continue
		}

		dir := filepath.Join(m.cfg.Directory, name)
		if _, err := os.Stat(filepath.Join(dir, ManifestName)); err != nil {
			continue
		}

		found[name] = true

		m.mu.RLock()
		_, known := m.instances[name]
		m.mu.RUnlock()
		if known {
			continue
		}

		manifest, err := LoadManifest(dir)
		if err != nil {
			m.log().WithFields(log.Fields{"plugin": name, "error": err}).
				Error("plugin manifest could not be read")

			m.addInstance(brokenInstance(name, dir, err))
			continue
		}

		m.addInstance(m.newInstance(manifest))
	}

	// Forget plugins whose directory has been removed from under us, which is
	// how an operator uninstalls one by hand.
	m.mu.Lock()
	for id, inst := range m.instances {
		if found[id] {
			continue
		}
		if inst.Loaded() {
			// Unload outside the lock would be cleaner, but this only happens
			// when a directory disappears while the plugin is running, and
			// leaving it dispatched to would be worse.
			go inst.unload()
		}
		delete(m.instances, id)
	}
	m.mu.Unlock()

	m.resort()
	m.refreshDispatch()
	return nil
}

// newInstance builds an unloaded instance around a manifest.
func (m *Manager) newInstance(manifest *Manifest) *Instance {
	inst := &Instance{
		manifest:    manifest,
		bridge:      m.bridge,
		gateTimeout: m.gateTimeout(),
		store: &store{
			db:       m.db,
			pluginID: manifest.ID,
		},
		fetcher: newFetcher(manifest.ID, PluginHTTPLimits{
			AllowedHosts:     m.cfg.AllowedHosts,
			MaxResponseBytes: m.cfg.MaxResponseSize * 1024 * 1024,
			Timeout:          time.Duration(m.cfg.RequestTimeout) * time.Second,
		}),
		onStatusChange: m.persistStatus,
	}
	inst.host = &host{instance: inst, bridge: m.bridge}
	return inst
}

// brokenInstance represents a plugin whose manifest could not be read, so that
// it still shows up in the Panel with a reason instead of disappearing.
func brokenInstance(id, dir string, cause error) *Instance {
	return &Instance{
		manifest: &Manifest{
			ID:          id,
			Name:        id,
			Author:      "unknown",
			Version:     "0.0.0",
			Description: "This plugin's plugin.json could not be read.",
			Category:    CategoryPlugin,
			dir:         dir,
			Meta: Meta{
				Status:        StatusErrored,
				StatusMessage: cause.Error(),
			},
		},
	}
}

func (m *Manager) addInstance(inst *Instance) {
	m.mu.Lock()
	m.instances[inst.manifest.ID] = inst
	m.mu.Unlock()
}

func (m *Manager) gateTimeout() time.Duration {
	if m.cfg.GateTimeout <= 0 {
		return DefaultGateTimeout
	}
	return time.Duration(m.cfg.GateTimeout) * time.Second
}

// resort recomputes the dispatch order.
func (m *Manager) resort() {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(a, b int) bool {
		ia := m.instances[ids[a]].manifest.Meta.LoadOrder
		ib := m.instances[ids[b]].manifest.Meta.LoadOrder
		if ia != ib {
			return ia < ib
		}
		// Ties break on id so the order is stable across restarts rather than
		// depending on map iteration.
		return ids[a] < ids[b]
	})

	m.order = ids
}

// All returns every plugin on the node in dispatch order.
func (m *Manager) All() []*Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*Instance, 0, len(m.order))
	for _, id := range m.order {
		if inst, ok := m.instances[id]; ok {
			out = append(out, inst)
		}
	}
	return out
}

// Get returns one plugin by id.
func (m *Manager) Get(id string) (*Instance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[strings.ToLower(id)]
	return inst, ok
}

// active returns the plugins that are loaded and enabled, in dispatch order.
// This is the list every hook dispatch walks, so it avoids allocating when
// there is nothing to dispatch to.
func (m *Manager) active() []*Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []*Instance
	for _, id := range m.order {
		inst, ok := m.instances[id]
		if !ok {
			continue
		}
		if inst.manifest.Meta.Status == StatusEnabled && inst.interpreter != nil {
			out = append(out, inst)
		}
	}
	return out
}

// LoadEnabled loads every plugin that is marked enabled.
//
// One plugin failing to load does not stop the others: it is marked errored
// with the reason and the rest carry on, because a node with three working
// plugins and one broken one should boot with three working plugins.
func (m *Manager) LoadEnabled() error {
	if !m.cfg.Enabled {
		m.log().Debug("plugin subsystem is disabled; not loading any plugins")
		return nil
	}

	for _, inst := range m.All() {
		if inst.Status() != StatusEnabled {
			continue
		}
		if err := m.loadInstance(inst); err != nil {
			m.log().WithFields(log.Fields{"plugin": inst.ID(), "error": err}).
				Error("failed to load plugin")
			continue
		}
		m.log().WithFields(log.Fields{
			"plugin":  inst.ID(),
			"version": inst.Manifest().Version,
		}).Info("loaded plugin")
	}

	return nil
}

// loadInstance checks compatibility and then loads the plugin, recording the
// outcome in its manifest.
func (m *Manager) loadInstance(inst *Instance) error {
	manifest := inst.Manifest()

	// Refuse a plugin written against a newer contract rather than loading it
	// and failing on whichever hook changed.
	if !manifest.APIVersionSupported() {
		msg := "This plugin needs plugin API version " +
			strconv.Itoa(manifest.EffectiveAPIVersion()) +
			" but this Wings only supports up to version " + strconv.Itoa(api.Version) + "."
		m.persistStatus(inst, StatusIncompatible, msg)
		return errors.New(msg)
	}

	if !manifest.Compatible() {
		msg := "This plugin is only compatible with Wings version " + manifest.WingsVersion
		if !manifest.WingsVersionStrict() {
			msg += " or a compatible newer version"
		}
		msg += "."
		m.persistStatus(inst, StatusIncompatible, msg)
		return errors.New(msg)
	}

	trusted := m.isTrusted(manifest.ID)
	if trusted {
		m.log().WithField("plugin", manifest.ID).
			Warn("loading plugin against the full standard library because the node config lists it as trusted")
	}

	// Lay out the plugin's declared Go modules before interpreting it, so an
	// import resolves. A plugin whose libraries cannot be fetched is refused
	// with the reason rather than loaded without them, which would fail later
	// and less clearly.
	goPath, err := m.ensureDependencies(inst)
	if err != nil {
		m.persistStatus(inst, StatusErrored, err.Error())
		return err
	}

	if err := inst.load(trusted, goPath); err != nil {
		m.persistStatus(inst, StatusErrored, err.Error())
		return err
	}

	// A plugin that previously errored and now loads cleanly is returned to
	// enabled, so recovering from a bad version does not need an operator to
	// click anything.
	if manifest.Meta.Status != StatusEnabled {
		m.persistStatus(inst, StatusEnabled, "")
	}

	m.syncDeclaredSettings(inst)
	m.registerExtensions(inst)
	m.refreshDispatch()
	return nil
}

// syncDeclaredSettings records the settings schema a freshly loaded plugin
// declared in its code.
//
// The code is the source of truth, but the Panel needs a schema to render a
// form for a plugin that is currently disabled, and a disabled plugin is not
// loaded to ask. Writing the declaration into the manifest on every load keeps
// both true: an operator can configure a plugin that is switched off, and the
// schema they see is the one the plugin last declared.
func (m *Manager) syncDeclaredSettings(inst *Instance) {
	declared := settingsFromRegistration(inst.Registration().Settings)

	inst.mu.Lock()
	unchanged := sameSettings(inst.manifest.Settings, declared)
	if !unchanged {
		inst.manifest.Settings = declared
	}
	manifest := inst.manifest
	inst.mu.Unlock()

	// Only write when it actually changed, so enabling a plugin does not
	// rewrite its manifest for nothing.
	if unchanged {
		return
	}

	if err := manifest.Save(); err != nil {
		m.log().WithFields(log.Fields{"plugin": manifest.ID, "error": err}).
			Error("could not record the plugin's declared settings")
	}
}

// isTrusted reports whether the node config vouches for this plugin.
func (m *Manager) isTrusted(id string) bool {
	for _, t := range m.cfg.Trusted {
		if strings.EqualFold(strings.TrimSpace(t), id) {
			return true
		}
	}
	return false
}

// persistStatus writes a status to the manifest on disk and in memory.
//
// Keeping the two in step here, rather than at each call site, is what makes
// the status the Panel reads the same one dispatch consults.
func (m *Manager) persistStatus(inst *Instance, status Status, message string) {
	inst.mu.Lock()
	inst.manifest.Meta.Status = status
	inst.manifest.Meta.StatusMessage = message
	manifest := inst.manifest
	inst.mu.Unlock()

	// The dispatch set is keyed on status, so recompute it before touching
	// disk: a plugin that just errored must stop receiving hooks immediately,
	// whether or not the manifest write succeeds.
	m.refreshDispatch()

	if manifest.dir == "" {
		return
	}
	if err := manifest.Save(); err != nil {
		m.log().WithFields(log.Fields{"plugin": manifest.ID, "error": err}).
			Error("could not write plugin status to its manifest")
	}
}

// Install accepts a plugin that is on disk but has never been set up.
//
// For Wings this means proving the plugin actually works: its manifest is
// valid, its source interprets, its entrypoint has the right shape, and what it
// registers is mountable. A plugin that cannot pass that is left not installed
// with the reason attached, rather than enabled and failing later.
func (m *Manager) Install(id string, enable bool) error {
	inst, ok := m.Get(id)
	if !ok {
		return errors.Errorf("plugins: %s is not on this node", id)
	}

	if inst.Status() != StatusNotInstalled {
		return errors.Errorf("plugins: %s is already installed", id)
	}

	if err := m.loadInstance(inst); err != nil {
		return err
	}

	if !enable {
		// Loading proved it works; now put it back down until an operator
		// asks for it.
		m.unregisterExtensions(inst)
		inst.unload()
		m.persistStatus(inst, StatusDisabled, "")
	}

	m.refreshDispatch()
	return nil
}

// Enable loads a disabled plugin and starts dispatching to it.
func (m *Manager) Enable(id string) error {
	inst, ok := m.Get(id)
	if !ok {
		return errors.Errorf("plugins: %s is not on this node", id)
	}

	switch inst.Status() {
	case StatusEnabled:
		if inst.Loaded() {
			return errors.Errorf("plugins: %s is already enabled", id)
		}
		// Marked enabled but not loaded, which is what a plugin looks like
		// after a failed boot. Loading it is exactly the right thing to do.
	case StatusNotInstalled:
		return errors.Errorf("plugins: %s has not been installed yet", id)
	}

	return m.loadInstance(inst)
}

// Disable unloads a plugin and stops dispatching to it.
//
// This takes effect immediately, without restarting Wings: the plugin's
// Shutdown hook runs, its interpreter is discarded, and its routes, jobs,
// parsers and backup adapter are unregistered.
func (m *Manager) Disable(id string) error {
	inst, ok := m.Get(id)
	if !ok {
		return errors.Errorf("plugins: %s is not on this node", id)
	}

	if inst.Status() == StatusNotInstalled {
		return errors.Errorf("plugins: %s has not been installed yet", id)
	}

	m.unregisterExtensions(inst)
	inst.unload()
	m.persistStatus(inst, StatusDisabled, "")
	m.refreshDispatch()
	return nil
}

// Uninstall returns a plugin to the not-installed state, optionally deleting
// its files.
//
// The plugin's stored keys go either way: storage belongs to an installed
// plugin, and leaving rows behind for a plugin that is no longer set up would
// mean a later reinstall silently inherited stale state.
func (m *Manager) Uninstall(id string, deleteFiles bool) error {
	inst, ok := m.Get(id)
	if !ok {
		return errors.Errorf("plugins: %s is not on this node", id)
	}

	if inst.Status() == StatusNotInstalled && !deleteFiles {
		return errors.Errorf("plugins: %s is not installed", id)
	}

	m.unregisterExtensions(inst)
	inst.unload()

	if inst.store != nil {
		if err := inst.store.purge(); err != nil {
			m.log().WithFields(log.Fields{"plugin": id, "error": err}).
				Warn("could not clear the plugin's stored data")
		}
	}

	if deleteFiles {
		dir := inst.Manifest().Directory()

		// Refuse to remove anything that is not actually a plugin directory
		// under the configured root. A manifest with a doctored directory
		// must not be able to turn an uninstall into a recursive delete
		// somewhere else on the node.
		if err := m.assertInsidePluginDirectory(dir); err != nil {
			return err
		}

		if err := os.RemoveAll(dir); err != nil {
			return errors.Wrapf(err, "plugins: could not delete %s", dir)
		}

		m.mu.Lock()
		delete(m.instances, inst.manifest.ID)
		m.mu.Unlock()
		m.resort()
		m.refreshDispatch()
		return nil
	}

	m.persistStatus(inst, StatusNotInstalled, "")
	m.refreshDispatch()
	return nil
}

// assertInsidePluginDirectory checks that dir is a direct child of the plugin
// root, with symlinks resolved, before anything destructive happens to it.
func (m *Manager) assertInsidePluginDirectory(dir string) error {
	root, err := filepath.EvalSymlinks(m.cfg.Directory)
	if err != nil {
		return errors.Wrap(err, "plugins: could not resolve the plugin directory")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return errors.Wrapf(err, "plugins: could not resolve %s", dir)
	}

	if filepath.Dir(resolved) != root {
		return errors.Errorf("plugins: refusing to delete %s because it is not inside %s", resolved, root)
	}
	return nil
}

// SaveSettings stores new settings for a plugin and tells it they changed.
//
// Only keys the manifest declares are accepted. An undeclared key is a sign the
// Panel and the plugin disagree about the plugin's own shape, and writing it
// would leave a value nothing ever reads.
func (m *Manager) SaveSettings(id string, values map[string]any) error {
	inst, ok := m.Get(id)
	if !ok {
		return errors.Errorf("plugins: %s is not on this node", id)
	}

	manifest := inst.Manifest()
	declared := make(map[string]bool, len(manifest.Settings))
	for _, s := range manifest.Settings {
		declared[s.Key] = true
	}

	var unknown []string
	for k := range values {
		if !declared[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return errors.Errorf("plugins: %s does not declare the setting(s) %s", id, strings.Join(unknown, ", "))
	}

	inst.mu.Lock()
	if inst.manifest.Meta.Settings == nil {
		inst.manifest.Meta.Settings = map[string]any{}
	}
	for k, v := range values {
		inst.manifest.Meta.Settings[k] = v
	}
	saved := inst.manifest
	inst.mu.Unlock()

	if err := saved.Save(); err != nil {
		return err
	}

	// The plugin sees the new values through api.Settings whether or not it
	// registered the hook, because settings read through to the manifest. The
	// hook is only the notification.
	reg := inst.Registration()
	if reg.SettingsChanged != nil {
		var hookErr error
		if inst.invoke("SettingsChanged", inst.gateTimeout, func() {
			hookErr = reg.SettingsChanged(inst.host.Settings())
		}) && hookErr != nil {
			m.persistStatus(inst, StatusEnabled, "The plugin rejected the new settings: "+hookErr.Error())
			return errors.Wrap(hookErr, "plugins: the plugin rejected the new settings")
		}
	}

	return nil
}

// SetLoadOrder reorders plugins, which reorders hook dispatch.
//
// Ids not named keep their relative position after the ones that were, so
// reordering two plugins does not require sending the whole list.
func (m *Manager) SetLoadOrder(ids []string) error {
	m.mu.RLock()
	unknown := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := m.instances[strings.ToLower(id)]; !ok {
			unknown = append(unknown, id)
		}
	}
	m.mu.RUnlock()

	if len(unknown) > 0 {
		return errors.Errorf("plugins: no such plugin(s): %s", strings.Join(unknown, ", "))
	}

	for i, id := range ids {
		inst, ok := m.Get(id)
		if !ok {
			continue
		}
		inst.mu.Lock()
		inst.manifest.Meta.LoadOrder = i
		manifest := inst.manifest
		inst.mu.Unlock()

		if err := manifest.Save(); err != nil {
			return err
		}
	}

	m.resort()
	m.refreshDispatch()
	return nil
}

// Shutdown unloads every loaded plugin, running each Shutdown hook. Called when
// Wings is stopping so plugins get the chance to flush state.
func (m *Manager) Shutdown() {
	for _, inst := range m.All() {
		if inst.Loaded() {
			m.unregisterExtensions(inst)
			inst.unload()
		}
	}

	m.refreshDispatch()
}

// IsTrusted reports whether the node's configuration vouches for a plugin,
// which is what decides whether it is loaded against the full standard library.
// Exported for the management API, so an operator can see at a glance which
// plugins are running without the usual restrictions.
func (m *Manager) IsTrusted(id string) bool {
	return m.isTrusted(id)
}
