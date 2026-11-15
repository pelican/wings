package plugin

import (
	"fmt"
	"os"

	"github.com/apex/log"

	"github.com/pelican/wings/config"
)

// PanelAccess is how much the Panel may do with plugins on this node, set by
// the node operator (plugins.panel_access, or WINGS_PLUGIN_PANEL_ACCESS).
// External plugins run on the node as the Wings user, so letting the Panel put
// new code there is the operator's decision, like allowed_mounts.
type PanelAccess string

const (
	// PanelAccessOff refuses every plugin request from the Panel.
	PanelAccessOff PanelAccess = "off"
	// PanelAccessView lets the Panel see plugins and enable, disable and
	// configure the ones the node already trusts. No new code reaches the
	// node. The default.
	PanelAccessView PanelAccess = "view"
	// PanelAccessSigned also lets the Panel install, update and uninstall
	// plugins from the node's configured registries, when the archive is
	// signed by a key in plugins.trusted_keys.
	PanelAccessSigned PanelAccess = "signed"
	// PanelAccessFull also lets the Panel install from any URL, an uploaded
	// archive, or without a signature. Panel admins can then run code on
	// this node.
	PanelAccessFull PanelAccess = "full"
)

// PanelAction is one kind of plugin request from the Panel.
type PanelAction string

const (
	// ActionList: list plugins, show one, read its files.
	ActionList PanelAction = "list"
	// ActionToggle: enable or disable an installed, trusted plugin.
	ActionToggle PanelAction = "toggle"
	// ActionSettings: change an installed plugin's settings.
	ActionSettings PanelAction = "settings"
	// ActionInstallSigned: install, update or uninstall from a configured
	// registry; the archive must be signed by a trusted key.
	ActionInstallSigned PanelAction = "install-signed"
	// ActionInstallAny: install or update from any URL or an uploaded
	// archive, signed or not.
	ActionInstallAny PanelAction = "install-any"
)

// PanelAccessEnv overrides plugins.panel_access, for fleets configured
// through the environment.
const PanelAccessEnv = "WINGS_PLUGIN_PANEL_ACCESS"

// ParsePanelAccess reads a level name.
func ParsePanelAccess(s string) (PanelAccess, error) {
	switch a := PanelAccess(s); a {
	case PanelAccessOff, PanelAccessView, PanelAccessSigned, PanelAccessFull:
		return a, nil
	}
	return "", fmt.Errorf("plugin: unknown panel access %q (want off, view, signed or full)", s)
}

// Allows reports whether this level permits the action.
func (a PanelAccess) Allows(act PanelAction) bool {
	switch act {
	case ActionList, ActionToggle, ActionSettings:
		return a == PanelAccessView || a == PanelAccessSigned || a == PanelAccessFull
	case ActionInstallSigned:
		return a == PanelAccessSigned || a == PanelAccessFull
	case ActionInstallAny:
		return a == PanelAccessFull
	}
	return false
}

// CurrentPanelAccess returns the level in effect: WINGS_PLUGIN_PANEL_ACCESS
// if set, else plugins.panel_access. An unknown value is logged and treated
// as "off", so a typo never grants more than intended.
func CurrentPanelAccess() PanelAccess {
	raw, source := config.Get().Plugins.PanelAccess, "plugins.panel_access"
	if env, ok := os.LookupEnv(PanelAccessEnv); ok {
		raw, source = env, PanelAccessEnv
	}
	if raw == "" {
		return PanelAccessView
	}
	a, err := ParsePanelAccess(raw)
	if err != nil {
		log.WithField("source", source).WithField("error", err).Warn("invalid plugin panel access; refusing Panel plugin requests")
		return PanelAccessOff
	}
	return a
}

// AfterPolicyChange, if set, runs after ApplyPolicy changes the plugins block
// in memory. The daemon sets it to load external plugins that just became
// enabled.
var AfterPolicyChange func()

// ApplyPolicy makes p the running daemon's plugins block and runs
// AfterPolicyChange. Enable state is read live by event delivery, plugin
// routes and plugin tasks, so enabling or disabling takes effect at once.
// Callers have written p to the config file first (config.UpdatePluginsOnDisk).
func ApplyPolicy(p config.PluginConfiguration) {
	config.Update(func(c *config.Configuration) { c.Plugins = p })
	if AfterPolicyChange != nil {
		AfterPolicyChange()
	}
}
