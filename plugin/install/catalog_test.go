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
	unsafe := []string{"", ".", ".hidden", "../evil", "a/b", `a\b`, "x..y"}
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
