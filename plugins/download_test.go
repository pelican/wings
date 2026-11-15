package plugins

import (
	"strings"
	"testing"

	"github.com/pelican/wings/plugins/api"
)

const downloaderManifest = `{
    "id": "fetcher",
    "name": "Fetcher",
    "author": "Tests",
    "version": "1.0.0",
    "category": "plugin",
    "package": "fetcher",
    "meta": {"status": "not_installed", "load_order": 0}
}`

// downloaderSource exercises fetching files onto a server and extracting an
// archive, which is what lets a plugin install something without the bytes
// passing through the Panel.
const downloaderSource = `
package fetcher

import (
	"strconv"
	"strings"

	"github.com/pelican/wings/plugins/api"
)

type plugin struct {
	host api.Host
}

func (p *plugin) init(h api.Host) error {
	p.host = h
	return nil
}

// batch fetches every url given in the "urls" query parameter and reports how
// many succeeded, so a test can see partial failure handled rather than the
// whole batch collapsing.
func (p *plugin) batch(r api.Request) api.Response {
	raw := ""
	if v, ok := r.Query["urls"]; ok && len(v) > 0 {
		raw = v[0]
	}

	reqs := []api.DownloadRequest{}
	for _, u := range strings.Split(raw, ",") {
		if u != "" {
			reqs = append(reqs, api.DownloadRequest{URL: u, Directory: "/mods"})
		}
	}

	results, err := p.host.Files().DownloadMany(r.Server.UUID, reqs, 4)
	if err != nil {
		return api.Text(500, "batch failed: "+err.Error())
	}

	ok, failed := 0, 0
	for _, res := range results {
		if res.Error != "" {
			failed++
			continue
		}
		ok++
	}

	return api.Text(200, "ok="+strconv.Itoa(ok)+" failed="+strconv.Itoa(failed))
}

// unpack fetches an archive and extracts it where it lands.
func (p *plugin) unpack(r api.Request) api.Response {
	url := ""
	if v, ok := r.Query["url"]; ok && len(v) > 0 {
		url = v[0]
	}

	result, err := p.host.Files().Download(r.Server.UUID, api.DownloadRequest{
		URL:       url,
		Directory: "/",
		FileName:  "pack.zip",
	})
	if err != nil {
		return api.Text(502, "download failed: "+err.Error())
	}

	if err := p.host.Files().Decompress(r.Server.UUID, result.Path); err != nil {
		return api.Text(500, "extract failed: "+err.Error())
	}

	return api.Text(200, "extracted "+result.Path)
}

func New() api.Registration {
	p := &plugin{}
	return api.Registration{
		Init: p.init,
		Routes: []api.Route{
			{Method: "GET", Path: "/batch", ServerScoped: true, Handler: p.batch},
			{Method: "GET", Path: "/unpack", ServerScoped: true, Handler: p.unpack},
		},
	}
}
`

func callRoute(t *testing.T, m *Manager, server api.Server, path string, query map[string][]string) api.Response {
	t.Helper()

	_, route, _, ok := m.FindRoute("fetcher", "GET", path, true)
	if !ok {
		t.Fatalf("route %s should resolve", path)
	}

	return route.Handler(api.Request{
		Method: "GET", Path: path, Query: query, Server: server, ServerScoped: true,
	})
}

func TestPluginDownloadsStraightOntoTheServer(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)
	enableTestPlugin(t, m, root, "fetcher", downloaderManifest, downloaderSource)

	res := callRoute(t, m, server, "/batch", map[string][]string{
		"urls": {"https://example.test/a.jar,https://example.test/b.jar,https://example.test/c.jar"},
	})

	if res.Status != 200 {
		t.Fatalf("batch returned %d: %s", res.Status, res.Body)
	}
	if got := string(res.Body); got != "ok=3 failed=0" {
		t.Fatalf("expected three downloads, got %q", got)
	}

	// The files are on the server, and the plugin never handled their bytes.
	for _, name := range []string{"/mods/a.jar", "/mods/b.jar", "/mods/c.jar"} {
		if _, err := bridge.FileRead(server.UUID, name); err != nil {
			t.Errorf("%s should be on the server, paths are %v", name, bridge.paths(server.UUID))
		}
	}

	// One request per file reached the node, rather than the plugin reading
	// and rewriting each one.
	if len(bridge.downloads) != 3 {
		t.Errorf("expected three downloads at the node, got %v", bridge.downloads)
	}
}

func TestOneBadDownloadDoesNotSinkTheBatch(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	// A modpack with one dead link should still install everything else.
	bridge.downloadErrors["https://example.test/gone.jar"] = "404 not found"

	m, root := newTestManager(t, bridge)
	enableTestPlugin(t, m, root, "fetcher", downloaderManifest, downloaderSource)

	res := callRoute(t, m, server, "/batch", map[string][]string{
		"urls": {"https://example.test/a.jar,https://example.test/gone.jar,https://example.test/c.jar"},
	})

	if got := string(res.Body); got != "ok=2 failed=1" {
		t.Fatalf("expected the batch to continue past the failure, got %q", got)
	}

	if _, err := bridge.FileRead(server.UUID, "/mods/c.jar"); err != nil {
		t.Error("the file after the failed one should still have been fetched")
	}
}

func TestPluginExtractsAnArchiveOnTheNode(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	// What the archive expands to once it is on the server.
	bridge.archives["/pack.zip"] = map[string][]byte{
		"manifest.json":      []byte(`{"files":[]}`),
		"overrides/cfg.toml": []byte("setting = true"),
	}

	m, root := newTestManager(t, bridge)
	enableTestPlugin(t, m, root, "fetcher", downloaderManifest, downloaderSource)

	res := callRoute(t, m, server, "/unpack", map[string][]string{
		"url": {"https://example.test/pack.zip"},
	})

	if res.Status != 200 {
		t.Fatalf("unpack returned %d: %s", res.Status, res.Body)
	}
	if !strings.Contains(string(res.Body), "/pack.zip") {
		t.Errorf("unexpected body %q", res.Body)
	}

	// Extraction lands beside the archive, which is the contract Wings
	// actually offers; there is no separate destination.
	for _, name := range []string{"/manifest.json", "/overrides/cfg.toml"} {
		if _, err := bridge.FileRead(server.UUID, name); err != nil {
			t.Errorf("%s should have been extracted, paths are %v", name, bridge.paths(server.UUID))
		}
	}
}
