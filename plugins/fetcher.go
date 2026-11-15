package plugins

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pelican/wings/plugins/api"
)

// fetcher is a plugin's outbound HTTP client.
//
// Plugins cannot open sockets themselves, so this is the only way one reaches
// the network. Going through here means each request is logged against the
// plugin that made it, is bounded in time and response size, and can be held
// to an operator's allowlist.
type fetcher struct {
	pluginID string
	client   *http.Client

	// allowedHosts is the operator's allowlist. Empty allows any host.
	allowedHosts []string

	// maxResponseBytes caps a response body.
	maxResponseBytes int64
}

var _ api.Fetcher = (*fetcher)(nil)

func newFetcher(pluginID string, cfg PluginHTTPLimits) *fetcher {
	return &fetcher{
		pluginID:         pluginID,
		allowedHosts:     cfg.AllowedHosts,
		maxResponseBytes: cfg.MaxResponseBytes,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

// PluginHTTPLimits is the outbound request policy for a plugin, derived from
// the node's configuration.
type PluginHTTPLimits struct {
	AllowedHosts     []string
	MaxResponseBytes int64
	Timeout          time.Duration
}

// Do performs an outbound request.
//
// A non-2xx status comes back as a status code rather than an error: whether a
// 404 is a problem is the plugin's call, not ours. An error means the request
// could not be made or completed at all.
func (f *fetcher) Do(method, rawURL string, body []byte, header map[string]string) (int, []byte, error) {
	if method == "" {
		method = http.MethodGet
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, errors.Wrap(err, "plugins: outbound request url is not valid")
	}

	// Only real HTTP. Without this a plugin could reach file:// or any other
	// scheme a transport happens to register, which would turn the one
	// controlled exit into an unrestricted one.
	if u.Scheme != "http" && u.Scheme != "https" {
		return 0, nil, errors.Errorf("plugins: outbound requests must be http or https, not %q", u.Scheme)
	}

	if !f.hostAllowed(u.Hostname()) {
		return 0, nil, errors.Errorf("plugins: %s is not in the node's plugin allowed_hosts list", u.Hostname())
	}

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(strings.ToUpper(method), rawURL, reader)
	if err != nil {
		return 0, nil, errors.Wrap(err, "plugins: could not build outbound request")
	}

	for k, v := range header {
		req.Header.Set(k, v)
	}
	// Identify the caller so the far end, and anyone reading its logs, can see
	// which plugin on which daemon is calling.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Pelican Wings plugin/"+f.pluginID)
	}

	log.WithFields(log.Fields{
		"subsystem": "plugins",
		"plugin":    f.pluginID,
		"method":    req.Method,
		"host":      u.Hostname(),
	}).Debug("plugin is making an outbound request")

	res, err := f.client.Do(req)
	if err != nil {
		return 0, nil, errors.Wrap(err, "plugins: outbound request failed")
	}
	defer res.Body.Close()

	// Read one byte past the cap so an over-sized body is detected rather than
	// silently truncated into something that might still parse.
	limited := io.LimitReader(res.Body, f.maxResponseBytes+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return res.StatusCode, nil, errors.Wrap(err, "plugins: could not read outbound response")
	}
	if int64(len(content)) > f.maxResponseBytes {
		return res.StatusCode, nil, errors.Errorf(
			"plugins: response from %s is over the %d byte limit for plugin requests",
			u.Hostname(), f.maxResponseBytes,
		)
	}

	return res.StatusCode, content, nil
}

// hostAllowed reports whether the allowlist permits a host. An empty allowlist
// permits everything.
func (f *fetcher) hostAllowed(host string) bool {
	if len(f.allowedHosts) == 0 {
		return true
	}

	host = strings.ToLower(strings.TrimSpace(host))
	for _, allowed := range f.allowedHosts {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == "" {
			continue
		}

		// A leading "*." covers subdomains, and also the bare domain, because
		// an operator writing "*.example.com" means the service rather than a
		// strict subdomain rule.
		if strings.HasPrefix(allowed, "*.") {
			suffix := allowed[1:]
			if host == allowed[2:] || strings.HasSuffix(host, suffix) {
				return true
			}
			continue
		}

		if host == allowed {
			return true
		}
	}
	return false
}
