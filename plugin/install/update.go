package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pelican/wings/plugin"
)

// Size and time limits for update fetches. The archive cap is deliberately
// generous but bounded so a hostile or broken update cannot exhaust disk.
const (
	maxUpdateManifestBytes = 64 << 10  // 64 KiB
	maxUpdateArchiveBytes  = 200 << 20 // 200 MiB
	maxUpdateFileBytes     = 100 << 20 // 100 MiB per archived file
	updateHTTPTimeout      = 60 * time.Second
)

// Archive-wide extraction limits. The download cap only bounds the compressed
// archive and the per-file cap only bounds one entry, so without these a small
// archive of many large or many tiny entries could fill the disk or its inodes.
// Variables so tests can lower them.
var (
	maxUpdateExtractBytes   int64 = 512 << 20 // 512 MiB in total
	maxUpdateExtractEntries       = 20000
)

var updateHTTPClient = &http.Client{Timeout: updateHTTPTimeout, CheckRedirect: checkPluginRedirect}

// maxPluginRedirects matches net/http's default redirect limit.
const maxPluginRedirects = 10

// checkPluginRedirect keeps every hop of a plugin fetch on https, so the scheme
// check in httpsGet cannot be bypassed by a redirect. When a redirect leaves the
// host the request was first sent to, every request header is dropped: net/http
// forwards custom headers to any host and Authorization to a same-domain host,
// and both carry private registry credentials.
func checkPluginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxPluginRedirects {
		return fmt.Errorf("plugin: stopped after %d redirects", maxPluginRedirects)
	}
	if req.URL.Scheme != "https" {
		return errors.New("plugin: refusing redirect to a non-https url")
	}
	if req.URL.Host != via[0].URL.Host {
		req.Header = make(http.Header)
	}
	return nil
}

// UpdateManifest is the JSON a plugin publisher hosts at Manifest.Update.
type UpdateManifest struct {
	Version   string `json:"version"`
	Download  string `json:"download"`  // https URL to the plugin archive
	SHA256    string `json:"sha256"`    // hex digest of the archive
	Signature string `json:"signature"` // base64 Ed25519 signature over the archive sha256
}

// UpdateStatus is the result of checking a plugin for updates.
type UpdateStatus struct {
	Name      string `json:"name"`
	Installed string `json:"installed"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`

	// Carried through to Apply; not printed.
	download  string
	sha256    string
	signature string
	dir       string
}

// CheckUpdate fetches the plugin's update manifest and reports whether a newer
// version is published. It performs no writes.
func CheckUpdate(ctx context.Context, m plugin.Manifest) (UpdateStatus, error) {
	if m.Update == "" {
		return UpdateStatus{}, fmt.Errorf("plugin %q declares no update URL", m.Name)
	}
	remote, err := fetchUpdateManifest(ctx, m.Update)
	if err != nil {
		return UpdateStatus{}, err
	}
	return UpdateStatus{
		Name:      m.Name,
		Installed: m.Version,
		Latest:    remote.Version,
		Available: versionNewer(remote.Version, m.Version),
		download:  remote.Download,
		sha256:    remote.SHA256,
		signature: remote.Signature,
		dir:       m.Dir(),
	}, nil
}

func fetchUpdateManifest(ctx context.Context, rawURL string) (UpdateManifest, error) {
	body, err := httpsGet(ctx, rawURL, maxUpdateManifestBytes, auth{})
	if err != nil {
		return UpdateManifest{}, err
	}
	defer body.Close()

	var um UpdateManifest
	if err := json.NewDecoder(body).Decode(&um); err != nil {
		return UpdateManifest{}, fmt.Errorf("plugin: decoding update manifest: %w", err)
	}
	if um.Version == "" || um.Download == "" || um.SHA256 == "" {
		return UpdateManifest{}, fmt.Errorf("plugin: update manifest missing version, download, or sha256")
	}
	return um, nil
}

// httpsGet performs a size-limited GET, requiring an https URL and attaching any
// registry auth. The caller must close the returned reader.
func httpsGet(ctx context.Context, rawURL string, maxBytes int64, a auth) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("plugin: invalid url: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("plugin: url must be https, got %q", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	a.apply(req)
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("plugin: fetching %s: %s", rawURL, resp.Status)
	}
	return &limitedReadCloser{r: io.LimitReader(resp.Body, maxBytes), c: resp.Body}, nil
}

type limitedReadCloser struct {
	r io.Reader
	c io.Closer
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l *limitedReadCloser) Close() error               { return l.c.Close() }

// versionNewer reports whether remote is a newer version than installed. It uses
// a simple numeric dot-separated comparison (e.g. 1.10.0 > 1.9.0); it does not
// interpret semver pre-release tags.
func versionNewer(remote, installed string) bool {
	return compareVersions(remote, installed) > 0
}

func compareVersions(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var an, bn int
		if i < len(as) {
			an = numericPrefix(as[i])
		}
		if i < len(bs) {
			bn = numericPrefix(bs[i])
		}
		if an != bn {
			if an > bn {
				return 1
			}
			return -1
		}
	}
	return 0
}

// numericPrefix parses the leading digits of a version segment (e.g. "3-beta"
// -> 3), returning 0 when there are none.
func numericPrefix(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}
