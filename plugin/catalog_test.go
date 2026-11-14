package plugin

import (
	"context"
	"testing"
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
