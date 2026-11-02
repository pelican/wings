package router

import (
	"emperror.dev/errors"
	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/server"
)

// fileGate offers a filesystem request to plugins before Wings performs it.
//
// It reports whether a plugin has taken the operation over, in which case the
// caller must skip its own work and treat the request as successful. A refusal
// comes back as an error, which the file endpoints already know how to turn
// into a response.
//
// Only requests that originate from outside reach here. Writes Wings performs
// for its own reasons, such as rendering egg configuration files on boot or
// unpacking an archive during a restore, go straight to the filesystem: a
// plugin refusing one of those would break the daemon rather than protect
// anything.
func fileGate(c *gin.Context, s *server.Server, action api.FileAction, path, target string, size int64, directory bool) (bool, error) {
	if !plugins.HasFileHooks() {
		return false, nil
	}

	allow, handled, reason := plugins.GateFileAction(api.FileEvent{
		Server:    s.PluginSnapshot(),
		Action:    action,
		Path:      path,
		Target:    target,
		Size:      size,
		Directory: directory,
		User:      requestUser(c),
		IP:        c.ClientIP(),
	})

	if !allow {
		return false, errors.New(reason)
	}
	return handled, nil
}

// fileObserved tells plugins a filesystem request succeeded.
func fileObserved(c *gin.Context, s *server.Server, action api.FileAction, path, target string, directory bool) {
	if !plugins.HasFileObservers() {
		return
	}

	plugins.FileAction(api.FileEvent{
		Server:    s.PluginSnapshot(),
		Action:    action,
		Path:      path,
		Target:    target,
		Directory: directory,
		User:      requestUser(c),
		IP:        c.ClientIP(),
	})
}

// requestUser returns the Panel user behind a request, when one is known.
//
// Most file endpoints are called by the Panel with the node token rather than
// on behalf of a signed-in user, so this is usually empty. The Panel sends the
// acting user on the endpoints where it has one, and a plugin should treat an
// empty value as "the Panel or the daemon did this", not as an anonymous user.
func requestUser(c *gin.Context) string {
	return c.GetHeader("X-Pelican-User")
}

// statForGate reports a path's size and whether it is a directory, for the
// benefit of a gate that wants to decide based on either.
//
// A path that cannot be stat'd reports an unknown size rather than failing the
// request: the operation itself will produce the real error a moment later, and
// reporting it from here would attribute it to the plugin subsystem.
func statForGate(s *server.Server, path string) (int64, bool) {
	st, err := s.Filesystem().UnixFS().Stat(path)
	if err != nil {
		return -1, false
	}
	return st.Size(), st.IsDir()
}
