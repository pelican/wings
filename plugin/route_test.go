package plugin

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterRoutesSetsOwnerAndCollects(t *testing.T) {
	before := len(Routes())

	noop := func(*gin.Context) {}
	RegisterRoutes("demo",
		Route{Scope: ScopePublic, Method: http.MethodGet, Path: "/health", Handler: noop},
		Route{Scope: ScopeServer, Method: http.MethodPost, Path: "/act", Handler: noop},
	)

	got := Routes()
	if len(got) != before+2 {
		t.Fatalf("expected %d routes, got %d", before+2, len(got))
	}
	for _, r := range got[before:] {
		if r.Plugin() != "demo" {
			t.Fatalf("expected route owner %q, got %q", "demo", r.Plugin())
		}
	}
	// Scopes are preserved as registered.
	if got[before].Scope != ScopePublic || got[before+1].Scope != ScopeServer {
		t.Fatalf("scopes not preserved: %v, %v", got[before].Scope, got[before+1].Scope)
	}
}
