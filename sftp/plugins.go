package sftp

import (
	"emperror.dev/errors"

	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
)

// gateFileAction offers an SFTP operation to plugins before it is carried out.
//
// SFTP is the half of the filesystem the Panel cannot see. A delete here never
// passes through the Panel's file manager, so a Panel-side plugin has no way to
// act on it, which is exactly the gap this gate closes: a recycle bin plugin
// gets the same chance to intercept an SFTP delete as it does a Panel one.
//
// It reports whether a plugin took the operation over. When it did, the caller
// must skip its own work and report success, because the plugin has already
// done something else with the file.
func (h *Handler) gateFileAction(action api.FileAction, path, target string, size int64, directory bool) (bool, error) {
	if !plugins.HasFileHooks() {
		return false, nil
	}

	allow, handled, reason := plugins.GateFileAction(api.FileEvent{
		Server:    h.server.PluginSnapshot(),
		Action:    action,
		Path:      path,
		Target:    target,
		Size:      size,
		Directory: directory,
		User:      h.events.user,
		IP:        h.events.ip,
	})

	if !allow {
		h.logger.WithField("path", path).WithField("reason", reason).
			Debug("a plugin refused an sftp operation")

		// Reported as a plain failure carrying the plugin's reason rather than
		// as a permission error. The user does have permission; something
		// chose not to allow this particular operation, and the reason is the
		// only way they will find that out over SFTP.
		return false, errors.New(reason)
	}

	return handled, nil
}

// observeFileAction tells plugins an SFTP operation succeeded.
func (h *Handler) observeFileAction(action api.FileAction, path, target string, directory bool) {
	if !plugins.HasFileObservers() {
		return
	}

	plugins.FileAction(api.FileEvent{
		Server:    h.server.PluginSnapshot(),
		Action:    action,
		Path:      path,
		Target:    target,
		Directory: directory,
		User:      h.events.user,
		IP:        h.events.ip,
	})
}

// sizeOf returns a path's size, or -1 when it cannot be determined.
//
// A gate is told how large the thing being deleted is so a plugin can decide
// whether it is worth keeping, and a failed stat should not stop the operation,
// so an unknown size is reported as -1 rather than as an error.
func (h *Handler) sizeOf(path string) (int64, bool) {
	st, err := h.fs.UnixFS().Stat(path)
	if err != nil {
		return -1, false
	}
	return st.Size(), st.IsDir()
}
