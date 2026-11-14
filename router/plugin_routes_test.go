package router

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
	"github.com/pelican/wings/server"
)

// TestPluginRouteScopesInheritCoreAuth builds the real router and checks that
// each plugin route scope is guarded by the same middleware as the core routes
// around it: public needs nothing, node needs the node token, and server needs
// the node token and a server that exists on this node.
func TestPluginRouteScopesInheritCoreAuth(t *testing.T) {
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")

	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationTokenId = "id"
	cfg.AuthenticationToken = "secret"
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	config.Set(cfg)

	ok := func(c *gin.Context) { c.String(http.StatusOK, "plugin") }
	plugin.RegisterRoutes("authcheck",
		plugin.Route{Scope: plugin.ScopePublic, Method: http.MethodGet, Path: "/ping", Handler: ok},
		plugin.Route{Scope: plugin.ScopeNode, Method: http.MethodGet, Path: "/ping", Handler: ok},
		plugin.Route{Scope: plugin.ScopeServer, Method: http.MethodGet, Path: "/ping", Handler: ok},
	)

	client := backupTestRemoteClient{credentials: make(chan [2]string, 1)}
	engine := Configure(server.NewEmptyManager(client), client)

	tests := []struct {
		name  string
		path  string
		token string
		want  int
	}{
		{"public without token", "/ext/authcheck/ping", "", http.StatusOK},
		{"node without token", "/api/ext/authcheck/ping", "", http.StatusUnauthorized},
		{"node with wrong token", "/api/ext/authcheck/ping", "nope", http.StatusForbidden},
		{"node with token", "/api/ext/authcheck/ping", "secret", http.StatusOK},
		{"server without token", "/api/servers/missing/ext/authcheck/ping", "", http.StatusUnauthorized},
		{"server with token, unknown server", "/api/servers/missing/ext/authcheck/ping", "secret", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("GET %s: status %d, want %d (body %q)", tt.path, rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}
