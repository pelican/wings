package middleware

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/config"
)

func requestWithNodeToken(t *testing.T, allowlist []string, remoteAddr string) int {
	t.Helper()
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")

	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationTokenId = "id"
	cfg.AuthenticationToken = "node-token"
	cfg.Api.PanelAllowlist = allowlist
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	config.Set(cfg)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/servers", RequireAuthorization(), func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	req.RemoteAddr = remoteAddr
	req.Header.Set("Authorization", "Bearer node-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

// Without an allowlist the node token is accepted from any address.
func TestRequireAuthorizationWithoutPanelAllowlist(t *testing.T) {
	if code := requestWithNodeToken(t, nil, "198.51.100.7:4321"); code != http.StatusOK {
		t.Fatalf("expected the request to be accepted, got %d", code)
	}
}

// With an allowlist the node token is only accepted from those addresses, so a
// leaked token cannot be used anywhere else.
func TestRequireAuthorizationWithPanelAllowlist(t *testing.T) {
	allowlist := []string{"10.0.0.0/8", "2001:db8::10"}

	accepted := []string{"10.1.2.3:4321", "[2001:db8::10]:4321"}
	for _, addr := range accepted {
		if code := requestWithNodeToken(t, allowlist, addr); code != http.StatusOK {
			t.Errorf("expected a request from %s to be accepted, got %d", addr, code)
		}
	}

	refused := []string{"198.51.100.7:4321", "[2001:db8::11]:4321", "11.0.0.1:4321"}
	for _, addr := range refused {
		if code := requestWithNodeToken(t, allowlist, addr); code != http.StatusForbidden {
			t.Errorf("expected a request from %s to be refused, got %d", addr, code)
		}
	}
}
