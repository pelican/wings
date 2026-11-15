package install

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"

	"github.com/pelican/wings/plugin"
)

// Remove deletes the plugin directory name from pluginsDir, under the install
// lock so it can't interleave with an install or update of the same plugin.
// The plugin's data directory (settings and state) is kept. Removing a plugin
// that is not there is not an error.
func Remove(pluginsDir, name string) error {
	if plugin.InvalidName(name) {
		return fmt.Errorf("plugin: invalid plugin name %q", name)
	}
	root, err := os.OpenRoot(pluginsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer root.Close()
	lock, err := lockInstalls(root)
	if err != nil {
		return err
	}
	s := &StagedInstall{lock: lock}
	defer s.unlock()
	if err := root.RemoveAll(name); err != nil {
		return fmt.Errorf("plugin: removing %s: %w", name, err)
	}
	return nil
}

// UseTransport makes plugin downloads use rt and returns a function that puts
// the previous transport back. It is for tests in other packages, which serve
// registries from an httptest TLS server.
func UseTransport(rt http.RoundTripper) (restore func()) {
	prev := updateHTTPClient.Transport
	updateHTTPClient.Transport = rt
	return func() { updateHTTPClient.Transport = prev }
}
