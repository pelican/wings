package plugin

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
	unsafe := []string{"", "../evil", "a/b", `a\b`, "x..y"}
	for _, name := range unsafe {
		err := Add(context.Background(),
			ResolvedPlugin{Entry: CatalogEntry{Name: name, Download: "https://example.com/a.tgz", SHA256: "abc"}},
			t.TempDir())
		if err == nil {
			t.Fatalf("expected rejection for unsafe name %q", name)
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
