package install

import (
	"net/http"
	"os"

	"github.com/pelican/wings/config"
)

// auth carries optional credentials for a private registry. The zero value adds
// no headers (public registries).
type auth struct {
	token   string
	headers map[string]string
}

// authFor builds auth from a registry config, expanding ${VAR} references so the
// actual secrets come from the environment, not the config file.
func authFor(r config.PluginRegistry) auth {
	a := auth{token: os.ExpandEnv(r.Token)}
	if len(r.Headers) > 0 {
		a.headers = make(map[string]string, len(r.Headers))
		for k, v := range r.Headers {
			a.headers[k] = os.ExpandEnv(v)
		}
	}
	return a
}

// apply attaches the credentials to an outbound request.
func (a auth) apply(req *http.Request) {
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}
}
