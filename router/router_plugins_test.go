package router

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
	"github.com/pelican/wings/plugin/install"
	"github.com/pelican/wings/server"
)

const alphaManifest = `apiVersion: v1
name: alpha
capability: events
version: 1.0.0
description: The first plugin.
entrypoint: ["./run"]
requires:
  hostAPI: ["server.logs"]
settings:
  - key: token
    type: string
    secret: true
  - key: level
    type: select
    options: [info, warn]
    default: info
  - key: retries
    type: number
`

// pluginAPI is a daemon with a config file on disk, a plugins directory
// holding a trusted "alpha" and an untrusted "beta", and the real router.
type pluginAPI struct {
	t          *testing.T
	engine     *gin.Engine
	dir        string
	configPath string
}

func newPluginAPI(t *testing.T, level string) *pluginAPI {
	t.Helper()
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")
	t.Setenv(plugin.PanelAccessEnv, level)

	base := t.TempDir()
	dir := filepath.Join(base, "plugins")
	path := filepath.Join(base, "config.yml")
	cfg, err := config.NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationTokenId = "id"
	cfg.AuthenticationToken = "secret"
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	cfg.Plugins.Directory = dir
	config.Set(cfg)
	if err := config.WriteToDisk(cfg); err != nil {
		t.Fatal(err)
	}

	a := &pluginAPI{t: t, dir: dir, configPath: path}
	a.writePlugin("alpha", alphaManifest)
	a.writePlugin("beta", strings.ReplaceAll(strings.Split(alphaManifest, "settings:")[0], "alpha", "beta"))
	m, err := plugin.LoadManifest(filepath.Join(dir, "alpha", plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := install.RecordTrust(m); err != nil {
		t.Fatal(err)
	}

	prevHook := plugin.AfterPolicyChange
	plugin.AfterPolicyChange = nil
	t.Cleanup(func() { plugin.AfterPolicyChange = prevHook })
	t.Cleanup(func() {
		pending.mu.Lock()
		for tok := range pending.entries {
			pending.dropLocked(tok)
		}
		pending.mu.Unlock()
	})

	client := backupTestRemoteClient{credentials: make(chan [2]string, 1)}
	a.engine = Configure(server.NewEmptyManager(client), client)
	return a
}

func (a *pluginAPI) writePlugin(name, manifest string) {
	a.t.Helper()
	pdir := filepath.Join(a.dir, name)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, plugin.ManifestFile), []byte(manifest), 0o644); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "run"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		a.t.Fatal(err)
	}
}

func (a *pluginAPI) do(method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer secret")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	a.engine.ServeHTTP(rec, req)
	return rec
}

func (a *pluginAPI) json(method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	a.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	rec := a.do(method, path, r, "application/json")
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// diskPlugins reads the plugins block from the config file.
func (a *pluginAPI) diskPlugins() config.PluginConfiguration {
	a.t.Helper()
	b, err := os.ReadFile(a.configPath)
	if err != nil {
		a.t.Fatal(err)
	}
	var f struct {
		Plugins config.PluginConfiguration `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		a.t.Fatal(err)
	}
	return f.Plugins
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestPluginAPIChecksTheAccessLevel(t *testing.T) {
	endpoints := []struct {
		method, path string
		need         plugin.PanelAccess
	}{
		{http.MethodGet, "/api/plugins", plugin.PanelAccessView},
		{http.MethodGet, "/api/plugins/alpha", plugin.PanelAccessView},
		{http.MethodGet, "/api/plugins/alpha/source", plugin.PanelAccessView},
		{http.MethodPost, "/api/plugins/alpha/enable", plugin.PanelAccessView},
		{http.MethodPost, "/api/plugins/alpha/disable", plugin.PanelAccessView},
		{http.MethodPut, "/api/plugins/alpha/settings", plugin.PanelAccessView},
		{http.MethodPost, "/api/plugins/order", plugin.PanelAccessView},
		{http.MethodPost, "/api/plugins/alpha/install", plugin.PanelAccessSigned},
		{http.MethodPost, "/api/plugins/alpha/update", plugin.PanelAccessSigned},
		{http.MethodPost, "/api/plugins/alpha/uninstall", plugin.PanelAccessSigned},
		{http.MethodPost, "/api/plugins/import", plugin.PanelAccessFull},
	}
	rank := map[plugin.PanelAccess]int{plugin.PanelAccessOff: 0, plugin.PanelAccessView: 1, plugin.PanelAccessSigned: 2, plugin.PanelAccessFull: 3}
	for _, level := range []plugin.PanelAccess{plugin.PanelAccessOff, plugin.PanelAccessView, plugin.PanelAccessSigned, plugin.PanelAccessFull} {
		for _, ep := range endpoints {
			t.Run(string(level)+" "+ep.method+" "+ep.path, func(t *testing.T) {
				a := newPluginAPI(t, string(level))
				before := a.diskPlugins()
				rec, body := a.json(ep.method, ep.path, "")
				allowed := rank[level] >= rank[ep.need]
				if allowed && rec.Code == http.StatusForbidden {
					t.Fatalf("refused at %s, which allows it: %s", level, rec.Body)
				}
				if !allowed {
					if rec.Code != http.StatusForbidden || body["required"] != string(ep.need) || body["panel_access"] != string(level) {
						t.Fatalf("status %d body %s; want 403 naming %q as required", rec.Code, rec.Body, ep.need)
					}
					after := a.diskPlugins()
					if len(after.Disabled) != len(before.Disabled) || len(after.Enabled) != len(before.Enabled) {
						t.Fatalf("a refused request changed the plugins block: %+v", after)
					}
					if _, err := os.Stat(filepath.Join(a.dir, "alpha", plugin.ManifestFile)); err != nil {
						t.Fatalf("a refused request touched the plugin: %v", err)
					}
				}
			})
		}
	}
}

func TestPluginAPIUnknownAccessLevelIsOff(t *testing.T) {
	a := newPluginAPI(t, "everything")
	if rec, _ := a.json(http.MethodGet, "/api/plugins", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("an unknown panel_access value should refuse everything, got %d", rec.Code)
	}
}

func TestPluginAPIRefusesAnEmptyOrInvalidID(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessFull))
	paths := []struct{ method, path string }{
		{http.MethodPost, "/api/plugins//uninstall?delete=true"},
		{http.MethodPost, "/api/plugins//disable"},
		{http.MethodPost, "/api/plugins//enable"},
		{http.MethodPut, "/api/plugins//settings"},
		{http.MethodGet, "/api/plugins//source"},
		{http.MethodPost, "/api/plugins//install"},
		{http.MethodPost, "/api/plugins//update"},
		{http.MethodPost, "/api/plugins/.trust.json/uninstall?delete=true"},
		{http.MethodPost, "/api/plugins/..%2F/uninstall?delete=true"},
		{http.MethodPost, "/api/plugins/../uninstall?delete=true"},
	}
	for _, p := range paths {
		rec, _ := a.json(p.method, p.path, "")
		if rec.Code < 300 {
			t.Errorf("%s %s: status %d, want it refused (body %s)", p.method, p.path, rec.Code, rec.Body)
		}
	}
	// The ones that reach the handlers are a 400.
	for _, p := range paths[:8] {
		if rec, _ := a.json(p.method, p.path, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status %d, want 400 (body %s)", p.method, p.path, rec.Code, rec.Body)
		}
	}
	for _, name := range []string{"alpha", "beta", ".trust.json"} {
		if _, err := os.Stat(filepath.Join(a.dir, name)); err != nil {
			t.Fatalf("%s is gone from the plugins directory: %v", name, err)
		}
	}
	if p := a.diskPlugins(); contains(p.Disabled, "") || contains(p.Enabled, "") {
		t.Fatalf("an empty name was written to the plugins block: %+v", p)
	}
}

func TestPluginAPIListsPluginsForThePanel(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessView))
	m, _ := plugin.LoadManifest(filepath.Join(a.dir, "alpha", plugin.ManifestFile))
	if err := plugin.SaveSettings(m, map[string]any{"token": "hunter2", "retries": float64(2)}); err != nil {
		t.Fatal(err)
	}
	rec, body := a.json(http.MethodGet, "/api/plugins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatal("a secret setting value was shown")
	}
	if body["enabled"] != true || body["directory"] != a.dir || body["panel_access"] != "view" {
		t.Fatalf("top level = %v", body)
	}
	byID := map[string]map[string]any{}
	for _, p := range body["data"].([]any) {
		p := p.(map[string]any)
		byID[p["id"].(string)] = p
	}
	alpha, beta := byID["alpha"], byID["beta"]
	if alpha == nil || beta == nil {
		t.Fatalf("alpha and beta should be listed: %v", byID)
	}
	am := alpha["meta"].(map[string]any)
	if am["status"] != "enabled" || am["trust"] != "trusted" || am["is_trusted"] != true || am["can_disable"] != true || am["can_uninstall"] != false {
		t.Fatalf("alpha meta = %v", am)
	}
	values := am["values"].(map[string]any)
	if values["token"] != plugin.SecretMask || values["retries"] != float64(2) {
		t.Fatalf("alpha values = %v", values)
	}
	if alpha["description"] != "The first plugin." || alpha["api_version"] != float64(1) || alpha["category"] != "events" {
		t.Fatalf("alpha = %v", alpha)
	}
	if ha := alpha["host_access"].([]any); len(ha) != 1 || ha[0] != "server.logs" {
		t.Fatalf("alpha host_access = %v", ha)
	}
	if s := alpha["settings"].([]any); len(s) != 3 {
		t.Fatalf("alpha settings schema = %v", s)
	}
	bm := beta["meta"].(map[string]any)
	if bm["trust"] != "untrusted" || bm["status"] != "disabled" || bm["can_enable"] != false {
		t.Fatalf("beta meta = %v", bm)
	}

	if rec, one := a.json(http.MethodGet, "/api/plugins/alpha", ""); rec.Code != http.StatusOK || one["id"] != "alpha" {
		t.Fatalf("GET one: %d %v", rec.Code, one)
	}
	if rec, _ := a.json(http.MethodGet, "/api/plugins/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown: %d", rec.Code)
	}
}

func TestPluginAPIEnableDisableWritesTheConfigAndAppliesIt(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessView))
	calls := 0
	plugin.AfterPolicyChange = func() { calls++ }

	rec, body := a.json(http.MethodPost, "/api/plugins/alpha/disable", "")
	if rec.Code != http.StatusOK || body["status"] != "disabled" {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if !contains(a.diskPlugins().Disabled, "alpha") {
		t.Fatalf("config file plugins block = %+v, want alpha disabled", a.diskPlugins())
	}
	if plugin.Enabled("alpha") || !contains(config.Get().Plugins.Disabled, "alpha") {
		t.Fatal("the running daemon still has alpha enabled")
	}
	if calls != 1 {
		t.Fatalf("AfterPolicyChange ran %d times, want 1", calls)
	}

	rec, _ = a.json(http.MethodPost, "/api/plugins/alpha/enable", "")
	if rec.Code != http.StatusOK || contains(a.diskPlugins().Disabled, "alpha") || !plugin.Enabled("alpha") {
		t.Fatalf("enable: %d %s; disk %+v", rec.Code, rec.Body, a.diskPlugins())
	}

	// beta is not trusted: enabling it is refused and nothing is written.
	before, _ := os.ReadFile(a.configPath)
	if rec, _ := a.json(http.MethodPost, "/api/plugins/beta/enable", ""); rec.Code != http.StatusConflict {
		t.Fatalf("enabling an untrusted plugin: %d %s", rec.Code, rec.Body)
	}
	after, _ := os.ReadFile(a.configPath)
	if !bytes.Equal(before, after) {
		t.Fatal("a refused enable changed the config file")
	}
	// An unknown plugin can't be put in the config at all.
	if rec, _ := a.json(http.MethodPost, "/api/plugins/ghost/disable", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("disabling an unknown plugin: %d", rec.Code)
	}
}

func TestPluginAPISettingsAreValidated(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessView))
	tests := []struct {
		name, body, field string
	}{
		{"unknown key", `{"colour":"red"}`, "colour"},
		{"wrong type", `{"retries":"three"}`, "retries"},
		{"not an option", `{"level":"debug"}`, "level"},
	}
	for _, tt := range tests {
		rec, body := a.json(http.MethodPut, "/api/plugins/alpha/settings", tt.body)
		fields, _ := body["fields"].(map[string]any)
		if rec.Code != http.StatusUnprocessableEntity || fields[tt.field] == nil {
			t.Fatalf("%s: %d %s; want 422 naming %s", tt.name, rec.Code, rec.Body, tt.field)
		}
	}
	if rec, _ := a.json(http.MethodPut, "/api/plugins/alpha/settings", `"just a string"`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a non-object body: %d", rec.Code)
	}
	m, _ := plugin.LoadManifest(filepath.Join(a.dir, "alpha", plugin.ManifestFile))
	if got, _ := plugin.LoadSettings(m); len(got) != 0 {
		t.Fatalf("refused saves wrote %v", got)
	}

	rec, body := a.json(http.MethodPut, "/api/plugins/alpha/settings", `{"token":"s3cret","level":"warn","retries":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid save: %d %s", rec.Code, rec.Body)
	}
	if body["values"].(map[string]any)["token"] != plugin.SecretMask {
		t.Fatalf("the response showed the secret: %s", rec.Body)
	}
	// The form comes back with the mask; the secret is kept.
	if rec, _ := a.json(http.MethodPut, "/api/plugins/alpha/settings", `{"token":"********","level":"info","retries":4}`); rec.Code != http.StatusOK {
		t.Fatalf("save with masked secret: %d %s", rec.Code, rec.Body)
	}
	got, _ := plugin.LoadSettings(m)
	if got["token"] != "s3cret" || got["level"] != "info" || got["retries"] != float64(4) {
		t.Fatalf("stored = %v", got)
	}
	if info, err := os.Stat(filepath.Join(m.DataDir(), plugin.SettingsFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v %v", info, err)
	}

	if rec, _ := a.json(http.MethodPut, "/api/plugins/beta/settings", `{}`); rec.Code != http.StatusConflict {
		t.Fatalf("settings of an untrusted plugin: %d", rec.Code)
	}
}

func TestPluginAPISourceStaysInsideThePlugin(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessView))
	if err := os.Symlink(filepath.Join(a.dir, ".trust.json"), filepath.Join(a.dir, "alpha", "escape")); err != nil {
		t.Fatal(err)
	}
	rec, body := a.json(http.MethodGet, "/api/plugins/alpha/source", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	files := body["files"].([]any)
	if len(files) != 3 { // plugin.yaml, run, escape
		t.Fatalf("files = %v", files)
	}
	rec, body = a.json(http.MethodGet, "/api/plugins/alpha/source?file=plugin.yaml", "")
	if rec.Code != http.StatusOK || !strings.Contains(body["content"].(string), "name: alpha") {
		t.Fatalf("read plugin.yaml: %d %s", rec.Code, rec.Body)
	}
	for _, f := range []string{"escape", "../.trust.json", "/etc/passwd", "../beta/plugin.yaml"} {
		rec, _ := a.json(http.MethodGet, "/api/plugins/alpha/source?file="+f, "")
		if rec.Code == http.StatusOK {
			t.Fatalf("?file=%s was read: %s", f, rec.Body)
		}
	}
	big := bytes.Repeat([]byte("x"), maxSourceFileBytes+1)
	if err := os.WriteFile(filepath.Join(a.dir, "alpha", "big"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if rec, _ := a.json(http.MethodGet, "/api/plugins/alpha/source?file=big", ""); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a file over the cap: %d", rec.Code)
	}
}

func TestPluginAPIOrderIsNotImplemented(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessView))
	rec, body := a.json(http.MethodPost, "/api/plugins/order", `{"order":["alpha"]}`)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(body["error"].(string), "no load order") {
		t.Fatalf("order: %d %s", rec.Code, rec.Body)
	}
}

func TestPluginAPIUninstall(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessSigned))
	rec, body := a.json(http.MethodPost, "/api/plugins/alpha/uninstall?delete=false", "")
	if rec.Code != http.StatusOK || body["deleted"] != false {
		t.Fatalf("uninstall: %d %s", rec.Code, rec.Body)
	}
	ts, _ := plugin.LoadTrustStore()
	if ts.Approved["alpha"] != "" || !contains(a.diskPlugins().Disabled, "alpha") || plugin.Enabled("alpha") {
		t.Fatalf("alpha should be untrusted and disabled: trust %v, disk %+v", ts.Approved, a.diskPlugins())
	}
	if _, err := os.Stat(filepath.Join(a.dir, "alpha")); err != nil {
		t.Fatalf("delete=false removed the files: %v", err)
	}
	if rec, _ := a.json(http.MethodPost, "/api/plugins/alpha/uninstall?delete=maybe", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete=maybe: %d", rec.Code)
	}
	if rec, _ := a.json(http.MethodPost, "/api/plugins/alpha/uninstall?delete=true", ""); rec.Code != http.StatusOK {
		t.Fatalf("uninstall with delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("delete=true kept the files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "beta")); err != nil {
		t.Fatalf("another plugin was removed: %v", err)
	}
}

// --- installs ---------------------------------------------------------------

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gammaArchive(t *testing.T, version string) []byte {
	return tarGz(t, map[string]string{
		"plugin.yaml": "apiVersion: v1\nname: gamma\ncapability: events\nversion: " + version + "\nentrypoint: [\"./run\"]\nrequires:\n  hostAPI: [\"server.logs\"]\n",
		"run":         "#!/bin/sh\n",
	})
}

// registry serves a registry listing gamma with archive, signed by priv
// (unsigned if priv is empty), and configures it, with pub as a trusted key.
func (a *pluginAPI) registry(archive []byte, pub, priv string) {
	a.t.Helper()
	sig := ""
	if priv != "" {
		var err error
		if sig, err = install.SignArchive(priv, bytes.NewReader(archive)); err != nil {
			a.t.Fatal(err)
		}
	}
	sum := sha256.Sum256(archive)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tar.gz") {
			_, _ = w.Write(archive)
			return
		}
		_ = json.NewEncoder(w).Encode(install.Catalog{Name: "hub", Plugins: []install.CatalogEntry{{
			Name: "gamma", Version: "1.0.0", Capability: plugin.CapabilityEvents,
			Download: srv.URL + "/gamma.tar.gz", SHA256: hex.EncodeToString(sum[:]), Signature: sig,
		}}})
	}))
	a.t.Cleanup(srv.Close)
	a.t.Cleanup(install.UseTransport(srv.Client().Transport))
	c := config.Get()
	c.Plugins.Registries = map[string]config.PluginRegistry{"hub": {URL: srv.URL + "/registry.json"}}
	if pub != "" {
		c.Plugins.TrustedKeys = []string{"ops:" + pub}
	}
	config.Set(c)
}

func (a *pluginAPI) stagingDirs() []string {
	entries, _ := os.ReadDir(a.dir)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".wings-install-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestPluginAPIInstallNeedsApproval(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessSigned))
	pub, priv, _ := install.GenerateKeypair()
	a.registry(gammaArchive(t, "1.0.0"), pub, priv)
	loaded := 0
	plugin.AfterPolicyChange = func() { loaded++ }

	// Step one stages and shows what would be installed; nothing is in place.
	rec, staged := a.json(http.MethodPost, "/api/plugins/gamma/install", "[]")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("stage: %d %s", rec.Code, rec.Body)
	}
	token, _ := staged["approval_token"].(string)
	if token == "" || staged["signed"] != true || staged["plugin"].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("stage response = %v", staged)
	}
	if ha := staged["host_access"].([]any); len(ha) != 1 || ha[0] != "server.logs" {
		t.Fatalf("host_access = %v", ha)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "gamma")); !os.IsNotExist(err) {
		t.Fatal("staging installed the plugin")
	}
	if len(a.stagingDirs()) != 1 {
		t.Fatalf("staging dirs = %v", a.stagingDirs())
	}

	// A wrong token, or the right one on another endpoint, is refused and
	// leaves the stage waiting.
	for _, try := range []struct{ path, body string }{
		{"/api/plugins/gamma/install", `{"approve":"nope"}`},
		{"/api/plugins/gamma/update", `{"approve":"` + token + `"}`},
		{"/api/plugins/alpha/install", `{"approve":"` + token + `"}`},
	} {
		if rec, _ := a.json(http.MethodPost, try.path, try.body); rec.Code != http.StatusConflict {
			t.Fatalf("POST %s %s: %d %s; want 409", try.path, try.body, rec.Code, rec.Body)
		}
	}
	if _, err := os.Stat(filepath.Join(a.dir, "gamma")); !os.IsNotExist(err) {
		t.Fatal("a refused approval installed the plugin")
	}

	// Step two installs and trusts it.
	rec, done := a.json(http.MethodPost, "/api/plugins/gamma/install", `{"approve":"`+token+`"}`)
	if rec.Code != http.StatusOK || done["status"] != "installed" {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	m, err := plugin.LoadManifest(filepath.Join(a.dir, "gamma", plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := plugin.LoadTrustStore()
	if !ts.IsTrusted(m) {
		t.Fatal("the approved plugin is not trusted")
	}
	if loaded != 1 {
		t.Fatalf("AfterPolicyChange ran %d times, want 1", loaded)
	}
	if len(a.stagingDirs()) != 0 {
		t.Fatalf("staging dirs left: %v", a.stagingDirs())
	}
	// The token is single use.
	if rec, _ := a.json(http.MethodPost, "/api/plugins/gamma/install", `{"approve":"`+token+`"}`); rec.Code != http.StatusConflict {
		t.Fatalf("token reuse: %d %s", rec.Code, rec.Body)
	}
	// Installing again is an update's job.
	if rec, _ := a.json(http.MethodPost, "/api/plugins/gamma/install", ""); rec.Code != http.StatusConflict {
		t.Fatalf("install over an installed plugin: %d %s", rec.Code, rec.Body)
	}
}

func TestPluginAPIApprovalExpires(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessSigned))
	pub, priv, _ := install.GenerateKeypair()
	a.registry(gammaArchive(t, "1.0.0"), pub, priv)
	prev := pendingInstallTTL
	pendingInstallTTL = 100 * time.Millisecond
	t.Cleanup(func() { pendingInstallTTL = prev })

	rec, staged := a.json(http.MethodPost, "/api/plugins/gamma/install", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("stage: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(a.stagingDirs()) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(a.stagingDirs()) != 0 {
		t.Fatalf("the expired stage was not cleaned up: %v", a.stagingDirs())
	}
	rec, _ = a.json(http.MethodPost, "/api/plugins/gamma/install", `{"approve":"`+staged["approval_token"].(string)+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expired token: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "gamma")); !os.IsNotExist(err) {
		t.Fatal("an expired approval installed the plugin")
	}
}

func TestPluginAPIApprovalIsTiedToTheStagedFiles(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessSigned))
	pub, priv, _ := install.GenerateKeypair()
	a.registry(gammaArchive(t, "1.0.0"), pub, priv)
	_, staged := a.json(http.MethodPost, "/api/plugins/gamma/install", "")
	dirs := a.stagingDirs()
	if len(dirs) != 1 {
		t.Fatalf("staging dirs = %v", dirs)
	}
	if err := os.WriteFile(filepath.Join(a.dir, dirs[0], "run"), []byte("#!/bin/sh\necho swapped\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec, _ := a.json(http.MethodPost, "/api/plugins/gamma/install", `{"approve":"`+staged["approval_token"].(string)+`"}`)
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "changed after the approval token") {
		t.Fatalf("approving changed files: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "gamma")); !os.IsNotExist(err) {
		t.Fatal("changed files were installed")
	}
}

func TestPluginAPISignedLevelNeedsATrustedSignature(t *testing.T) {
	_, otherPriv, _ := install.GenerateKeypair()
	pub, _, _ := install.GenerateKeypair()
	for name, priv := range map[string]string{"unsigned": "", "signed by an unknown key": otherPriv} {
		t.Run(name, func(t *testing.T) {
			a := newPluginAPI(t, string(plugin.PanelAccessSigned))
			a.registry(gammaArchive(t, "1.0.0"), pub, priv)
			// require_signatures is off; the signed level asks anyway.
			if config.Get().Plugins.RequireSignatures {
				t.Fatal("test expects require_signatures off")
			}
			rec, _ := a.json(http.MethodPost, "/api/plugins/gamma/install", "")
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("stage: %d %s; want 422", rec.Code, rec.Body)
			}
			if len(a.stagingDirs()) != 0 {
				t.Fatalf("a refused archive left %v", a.stagingDirs())
			}
		})
	}
	// At full, the node's own policy applies: unsigned is fine with
	// require_signatures off.
	a := newPluginAPI(t, string(plugin.PanelAccessFull))
	a.registry(gammaArchive(t, "1.0.0"), "", "")
	if rec, staged := a.json(http.MethodPost, "/api/plugins/gamma/install", ""); rec.Code != http.StatusAccepted || staged["signed"] != false {
		t.Fatalf("unsigned at full: %d %s", rec.Code, rec.Body)
	}
}

func TestPluginAPIUnsignedStageCantBeApprovedAfterLevelDrops(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessFull))
	a.registry(gammaArchive(t, "1.0.0"), "", "")
	_, staged := a.json(http.MethodPost, "/api/plugins/gamma/install", "")
	t.Setenv(plugin.PanelAccessEnv, string(plugin.PanelAccessSigned))
	rec, body := a.json(http.MethodPost, "/api/plugins/gamma/install", `{"approve":"`+staged["approval_token"].(string)+`"}`)
	if rec.Code != http.StatusForbidden || body["required"] != "full" {
		t.Fatalf("approve after the level dropped: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "gamma")); !os.IsNotExist(err) {
		t.Fatal("installed after the level dropped")
	}
	if len(a.stagingDirs()) != 0 {
		t.Fatalf("staging dirs left: %v", a.stagingDirs())
	}
}

func TestPluginAPIImportUpload(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessFull))
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("plugin", "gamma.tar.gz")
	_, _ = fw.Write(gammaArchive(t, "2.0.0"))
	_ = mw.Close()
	rec := a.do(http.MethodPost, "/api/plugins/import", &buf, mw.FormDataContentType())
	var staged map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &staged)
	if rec.Code != http.StatusAccepted || staged["plugin"].(map[string]any)["name"] != "gamma" {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	token := staged["approval_token"].(string)
	// An import token can't approve a registry install.
	if rec, _ := a.json(http.MethodPost, "/api/plugins/gamma/install", `{"approve":"`+token+`"}`); rec.Code != http.StatusConflict {
		t.Fatalf("import token on install: %d", rec.Code)
	}
	rec, done := a.json(http.MethodPost, "/api/plugins/import", `{"approve":"`+token+`"}`)
	if rec.Code != http.StatusOK || done["status"] != "installed" {
		t.Fatalf("approve import: %d %s", rec.Code, rec.Body)
	}
	m, err := plugin.LoadManifest(filepath.Join(a.dir, "gamma", plugin.ManifestFile))
	if err != nil || m.Version != "2.0.0" {
		t.Fatalf("installed = %+v %v", m, err)
	}
	if rec, _ := a.json(http.MethodPost, "/api/plugins/import", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("import with nothing: %d", rec.Code)
	}
}

// The expiry timer normally removes a stale stage; take refuses one whose
// time is up even if the timer has not run yet.
func TestPluginAPIRefusesAnExpiredTokenBeforeTheTimerRuns(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessSigned))
	pub, priv, _ := install.GenerateKeypair()
	a.registry(gammaArchive(t, "1.0.0"), pub, priv)
	_, staged := a.json(http.MethodPost, "/api/plugins/gamma/install", "")
	token := staged["approval_token"].(string)
	pending.mu.Lock()
	pending.entries[token].expires = time.Now().Add(-time.Second)
	pending.mu.Unlock()
	if rec, _ := a.json(http.MethodPost, "/api/plugins/gamma/install", `{"approve":"`+token+`"}`); rec.Code != http.StatusConflict {
		t.Fatalf("expired token: %d %s", rec.Code, rec.Body)
	}
	if len(a.stagingDirs()) != 0 {
		t.Fatalf("the expired stage was not removed: %v", a.stagingDirs())
	}
}

func TestPluginAPIUpdateNeedsApproval(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessSigned))
	pub, priv, _ := install.GenerateKeypair()
	c := config.Get()
	c.Plugins.TrustedKeys = []string{pub}
	config.Set(c)

	var archive []byte
	var sig string
	var sum [32]byte
	latest := "2.0.0"
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tar.gz") {
			_, _ = w.Write(archive)
			return
		}
		_ = json.NewEncoder(w).Encode(install.UpdateManifest{Version: latest, Download: srv.URL + "/delta.tar.gz", SHA256: hex.EncodeToString(sum[:]), Signature: sig})
	}))
	archive = tarGz(t, map[string]string{
		"plugin.yaml": "apiVersion: v1\nname: delta\ncapability: events\nversion: 2.0.0\nentrypoint: [\"./run\"]\nupdate: " + srv.URL + "/update.json\n",
		"run":         "#!/bin/sh\necho v2\n",
	})
	sig, _ = install.SignArchive(priv, bytes.NewReader(archive))
	sum = sha256.Sum256(archive)
	t.Cleanup(srv.Close)
	t.Cleanup(install.UseTransport(srv.Client().Transport))

	a.writePlugin("delta", "apiVersion: v1\nname: delta\ncapability: events\nversion: 1.0.0\nentrypoint: [\"./run\"]\nupdate: "+srv.URL+"/update.json\n")
	m, _ := plugin.LoadManifest(filepath.Join(a.dir, "delta", plugin.ManifestFile))
	if err := install.RecordTrust(m); err != nil {
		t.Fatal(err)
	}

	rec, staged := a.json(http.MethodPost, "/api/plugins/delta/update", "")
	if rec.Code != http.StatusAccepted || staged["replaces"] != "1.0.0" || staged["plugin"].(map[string]any)["version"] != "2.0.0" {
		t.Fatalf("stage update: %d %s", rec.Code, rec.Body)
	}
	if got, _ := plugin.LoadManifest(filepath.Join(a.dir, "delta", plugin.ManifestFile)); got.Version != "1.0.0" {
		t.Fatalf("staging replaced the installed version: %s", got.Version)
	}
	rec, done := a.json(http.MethodPost, "/api/plugins/delta/update", `{"approve":"`+staged["approval_token"].(string)+`"}`)
	if rec.Code != http.StatusOK || done["status"] != "updated" {
		t.Fatalf("approve update: %d %s", rec.Code, rec.Body)
	}
	m2, err := plugin.LoadManifest(filepath.Join(a.dir, "delta", plugin.ManifestFile))
	if err != nil || m2.Version != "2.0.0" {
		t.Fatalf("installed = %+v %v", m2, err)
	}
	ts, _ := plugin.LoadTrustStore()
	if !ts.IsTrusted(m2) {
		t.Fatal("the updated plugin is not trusted")
	}
	rec, body := a.json(http.MethodPost, "/api/plugins/delta/update", "")
	if rec.Code != http.StatusOK || body["status"] != "up_to_date" {
		t.Fatalf("update when current: %d %s", rec.Code, rec.Body)
	}
}

// A plugin's directory need not be named after it. delete=true removes the
// directory its manifest is in, and nothing that merely has its name.
func TestPluginAPIUninstallDeletesTheDirectoryHoldingTheManifest(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessSigned))
	a.writePlugin("notify-main", strings.ReplaceAll(strings.Split(alphaManifest, "settings:")[0], "alpha", "notify"))
	unrelated := filepath.Join(a.dir, "notify")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "keep.txt"), []byte("not a plugin"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, body := a.json(http.MethodPost, "/api/plugins/notify/uninstall?delete=true", "")
	if rec.Code != http.StatusOK || body["deleted"] != true {
		t.Fatalf("uninstall: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "notify-main")); !os.IsNotExist(err) {
		t.Fatalf("the plugin's own directory was kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unrelated, "keep.txt")); err != nil {
		t.Fatalf("a directory that only has the plugin's name was removed: %v", err)
	}
}
