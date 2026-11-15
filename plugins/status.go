package plugins

// Status is the state of a plugin on this node. The values match the Panel's
// own plugin statuses so the Panel can render a node's plugins with the same
// badges it uses for its own, without translating anything.
type Status string

const (
	// StatusNotInstalled means the plugin's files are on disk but it has never
	// been installed, so nothing has been loaded from it.
	StatusNotInstalled Status = "not_installed"

	// StatusDisabled means the plugin is installed and deliberately not
	// loaded.
	StatusDisabled Status = "disabled"

	// StatusEnabled means the plugin is loaded and its hooks are live.
	StatusEnabled Status = "enabled"

	// StatusErrored means loading the plugin failed, or it raised a panic
	// while running. Wings records why in the status message and stops calling
	// it.
	StatusErrored Status = "errored"

	// StatusIncompatible means the plugin declares a Wings version or plugin
	// API version this daemon cannot satisfy, so it was refused rather than
	// half-loaded.
	StatusIncompatible Status = "incompatible"
)

// Valid reports whether s is a status Wings recognises.
func (s Status) Valid() bool {
	switch s {
	case StatusNotInstalled, StatusDisabled, StatusEnabled, StatusErrored, StatusIncompatible:
		return true
	}
	return false
}

// Category groups plugins for display. It carries no behaviour: a plugin
// declaring the backup category is not thereby allowed to register a backup
// adapter, and one declaring "plugin" is not stopped from doing so.
//
// Wings deliberately has no theme or language category. Those exist on the
// Panel because the Panel renders a UI and ships translations; a daemon with
// neither has nothing to put in them.
type Category string

const (
	CategoryPlugin      Category = "plugin"
	CategoryBackup      Category = "backup"
	CategoryParser      Category = "parser"
	CategoryIntegration Category = "integration"
	CategorySecurity    Category = "security"
	CategoryMonitoring  Category = "monitoring"
)

// Valid reports whether c is a category Wings recognises.
func (c Category) Valid() bool {
	switch c {
	case CategoryPlugin, CategoryBackup, CategoryParser,
		CategoryIntegration, CategorySecurity, CategoryMonitoring:
		return true
	}
	return false
}
