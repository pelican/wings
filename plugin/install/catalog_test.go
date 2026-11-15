package install

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pelican/wings/config"
)

func TestCatalogFind(t *testing.T) {
	c := Catalog{Plugins: []CatalogEntry{
		{Name: "audit", Version: "1.0.0"},
		{Name: "backup", Version: "2.1.0"},
	}}
	if e, ok := c.find("backup"); !ok || e.Version != "2.1.0" {
		t.Fatalf("expected backup 2.1.0, got %+v ok=%v", e, ok)
	}
	if _, ok := c.find("nope"); ok {
		t.Fatal("did not expect to find nope")
	}
}

func TestAddRejectsUnsafeName(t *testing.T) {
	unsafe := []string{"", ".", ".hidden", "../evil", "a/b", `a\b`, "x..y", "available"}
	for _, name := range unsafe {
		_, err := StageAdd(context.Background(),
			ResolvedPlugin{Entry: CatalogEntry{Name: name, Download: "https://example.com/a.tgz", SHA256: "abc"}},
			t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "invalid plugin name") {
			t.Fatalf("expected %q to be refused as an invalid plugin name, got %v", name, err)
		}
	}
}

// registryServer serves a registry.json listing the given plugin names.
func registryServer(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	var entries []CatalogEntry
	for _, n := range names {
		entries = append(entries, CatalogEntry{Name: n, Version: "1.0.0", Download: "https://example.com/" + n + ".tar.gz", SHA256: "abc"})
	}
	body, err := json.Marshal(Catalog{Name: "test", Plugins: entries})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveBareNameMustBeUnambiguous(t *testing.T) {
	private := registryServer(t, "billing-hook", "only-private")
	public := registryServer(t, "billing-hook", "state-log")
	useTestTransport(t, private)

	c := setConfig(t)
	c.Plugins.Registries = map[string]config.PluginRegistry{
		"acme":      {URL: private.URL + "/registry.json"},
		"community": {URL: public.URL + "/registry.json"},
	}
	config.Set(c)
	ctx := context.Background()

	_, err := Resolve(ctx, "billing-hook")
	if err == nil || !strings.Contains(err.Error(), "@acme/billing-hook, @community/billing-hook") {
		t.Fatalf("a name in two registries must be refused and both listed, got %v", err)
	}
	if r, err := Resolve(ctx, "@acme/billing-hook"); err != nil || r.Entry.Name != "billing-hook" {
		t.Fatalf("@namespace/name should pick that registry, got %+v %v", r.Entry, err)
	}
	if r, err := Resolve(ctx, "state-log"); err != nil || r.Entry.Name != "state-log" {
		t.Fatalf("a name in one registry should resolve, got %+v %v", r.Entry, err)
	}
}

func TestResolveBareNameFailsWhenARegistryIsUnreadable(t *testing.T) {
	public := registryServer(t, "billing-hook")
	down := registryServer(t)
	downURL := down.URL
	down.Close()
	useTestTransport(t, public)

	c := setConfig(t)
	c.Plugins.Registries = map[string]config.PluginRegistry{
		"acme":      {URL: downURL + "/registry.json"},
		"community": {URL: public.URL + "/registry.json"},
	}
	config.Set(c)

	_, err := Resolve(context.Background(), "billing-hook")
	if err == nil || !strings.Contains(err.Error(), `could not read registry "acme"`) {
		t.Fatalf("an unreadable registry must fail a bare-name lookup, got %v", err)
	}
}

func TestRegistryCredentialsOnlyGoToTheRegistryHost(t *testing.T) {
	a := auth{token: "secret", headers: map[string]string{"X-Api-Key": "key"}}
	same := ResolvedPlugin{Entry: CatalogEntry{Download: "https://registry.acme.test/x.tar.gz"}, auth: a, registryHost: "registry.acme.test"}
	if got := same.downloadAuth(); got.token != "secret" || got.headers["X-Api-Key"] != "key" {
		t.Fatalf("an archive on the registry host should get its credentials, got %+v", got)
	}
	cdn := ResolvedPlugin{Entry: CatalogEntry{Download: "https://cdn.example.test/x.tar.gz"}, auth: a, registryHost: "registry.acme.test"}
	if got := cdn.downloadAuth(); got.token != "" || len(got.headers) != 0 {
		t.Fatalf("an archive on another host must not get registry credentials, got %+v", got)
	}
	port := ResolvedPlugin{Entry: CatalogEntry{Download: "https://registry.acme.test:8443/x.tar.gz"}, auth: a, registryHost: "registry.acme.test"}
	if got := port.downloadAuth(); got.token != "" {
		t.Fatalf("another port is another host, got %+v", got)
	}
}

// TestCatalogFindSkipsPanelEntries covers registries that list Panel and Wings
// plugins together (contract v1, section R1): Wings uses entries whose target
// is "wings" or missing, and never a "panel" one, even for a bare name.
func TestCatalogFindSkipsPanelEntries(t *testing.T) {
	var c Catalog
	if err := json.Unmarshal([]byte(`{"plugins":[
		{"name":"both","version":"1.0.0","target":"panel"},
		{"name":"both","version":"2.0.0","target":"wings"},
		{"name":"panel-only","version":"1.0.0","target":"panel"},
		{"name":"legacy","version":"3.0.0"}
	]}`), &c); err != nil {
		t.Fatal(err)
	}
	if e, ok := c.find("both"); !ok || e.Version != "2.0.0" {
		t.Fatalf("both = %+v, %v; want the wings entry 2.0.0", e, ok)
	}
	if _, ok := c.find("panel-only"); ok {
		t.Fatal("a target=panel entry must not be found by Wings")
	}
	if e, ok := c.find("legacy"); !ok || e.Version != "3.0.0" {
		t.Fatalf("an entry without target must count as wings, got %+v, %v", e, ok)
	}
}

// A signature field that is present but not a non-empty string makes the
// entry invalid; it never means "unsigned". The rest of the registry is
// still read.
func TestRegistryEntryWithAMalformedSignatureIsInvalid(t *testing.T) {
	body := `{"plugins":[
		{"name":"empty","version":"1","download":"https://example.com/a.tgz","sha256":"abc","signature":""},
		{"name":"null","version":"1","download":"https://example.com/a.tgz","sha256":"abc","signature":null},
		{"name":"number","version":"1","download":"https://example.com/a.tgz","sha256":"abc","signature":7},
		{"name":"unsigned","version":"1","download":"https://example.com/a.tgz","sha256":"abc"}
	]}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	useTestTransport(t, srv)
	for _, name := range []string{"empty", "null", "number"} {
		if _, err := Resolve(context.Background(), srv.URL+"/registry.json#"+name); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Errorf("%s: Resolve = %v, want the entry refused for its signature", name, err)
		}
	}
	if r, err := Resolve(context.Background(), srv.URL+"/registry.json#unsigned"); err != nil || r.Entry.Signature != "" {
		t.Fatalf("an entry without a signature field: %+v %v", r, err)
	}
}

// ListRegistries reads every configured registry with its credentials and
// keeps only what Wings installs; one that can't be read is reported, not
// fatal.
func TestListRegistries(t *testing.T) {
	var gotAuth string
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"plugins":[
			{"name":"state-log","version":"1.2.0","download":"https://example.com/a.tgz","sha256":"abc","signature":"c2ln"},
			{"name":"counter","version":"2.0.1","target":"panel","download":"https://example.com/b.zip","sha256":"abc"},
			{"name":"future","version":"1","target":"toaster","download":"https://example.com/c.zip","sha256":"abc"},
			{"name":"bad-sig","version":"1","download":"https://example.com/d.tgz","sha256":"abc","signature":""},
			{"name":"../evil","version":"1","download":"https://example.com/e.tgz","sha256":"abc"}
		]}`))
	}))
	t.Cleanup(good.Close)
	down := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	t.Cleanup(down.Close)
	useTestTransport(t, good)
	c, err := config.NewAtPath("test.yml")
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = "test-token"
	c.Plugins.Registries = map[string]config.PluginRegistry{
		"acme": {URL: good.URL + "/registry.json", Token: "t0ken"},
		"down": {URL: down.URL + "/registry.json"},
	}
	config.Set(c)

	got := ListRegistries(context.Background())
	if len(got) != 2 || got[0].Namespace != "acme" || got[1].Namespace != "down" {
		t.Fatalf("listings = %+v", got)
	}
	if got[0].Err != nil || len(got[0].Entries) != 1 || got[0].Entries[0].Name != "state-log" || got[0].Entries[0].Signature != "c2ln" {
		t.Fatalf("acme = %+v", got[0])
	}
	if gotAuth != "Bearer t0ken" {
		t.Fatalf("the registry's token was not sent: %q", gotAuth)
	}
	if got[1].Err == nil || len(got[1].Entries) != 0 {
		t.Fatalf("down = %+v; want an error and no entries", got[1])
	}
}

// TestResolveKeepsRegistryURLCredentialsOutOfErrors covers a registry URL
// that carries a key in its query string: a failed lookup's error reaches the
// Panel (POST /api/plugins/{id}/install), so it must not include the key.
func TestResolveKeepsRegistryURLCredentialsOutOfErrors(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	useTestTransport(t, srv)
	c := setConfig(t)
	c.Plugins.Registries = map[string]config.PluginRegistry{
		"acme": {URL: srv.URL + "/registry.json?key=supersecret"},
	}
	config.Set(c)

	for _, ref := range []string{"@acme/billing", "billing"} {
		_, err := Resolve(context.Background(), ref)
		if err == nil {
			t.Fatalf("Resolve(%q) succeeded against a failing registry", ref)
		}
		if strings.Contains(err.Error(), "supersecret") {
			t.Errorf("Resolve(%q) error shows the registry URL's key: %v", ref, err)
		}
	}
}
