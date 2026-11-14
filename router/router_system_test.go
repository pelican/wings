package router

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/server"
)

func TestPostUpdateConfigurationRotatesCredentials(t *testing.T) {
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")

	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationTokenId = "old-id"
	cfg.AuthenticationToken = "old-token"
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	config.Set(cfg)

	credentials := make(chan [2]string, 1)
	manager := server.NewEmptyManager(backupTestRemoteClient{credentials: credentials})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("manager", manager)
	c.Request = httptest.NewRequest("POST", "/api/update", strings.NewReader(`{"token_id":"new-id","token":"new-token"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	postUpdateConfiguration(c)

	if recorder.Code != 200 {
		t.Fatalf("expected successful update, got status %d", recorder.Code)
	}
	updated := config.Get()
	if updated.Token.ID != "new-id" || updated.Token.Token != "new-token" {
		t.Fatalf("unexpected resolved credentials: %#v", updated.Token)
	}
	select {
	case got := <-credentials:
		if got != [2]string{"new-id", "new-token"} {
			t.Fatalf("unexpected client credentials: %#v", got)
		}
	default:
		t.Fatal("expected client credentials to be rotated")
	}
}

// TestPostUpdateConfigurationKeepsPluginPolicy pins that a configuration push
// cannot change plugin policy. The plugins directory also holds the operator's
// trust approvals, so a push that moved it into a server's files would let that
// server's user get a plugin of their own run by Wings.
func TestPostUpdateConfigurationKeepsPluginPolicy(t *testing.T) {
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")

	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationTokenId = "id"
	cfg.AuthenticationToken = "token"
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	cfg.Plugins.Directory = "/var/lib/pelican/plugins"
	cfg.Plugins.Disabled = []string{"blocked"}
	cfg.Plugins.RequireSignatures = true
	config.Set(cfg)

	manager := server.NewEmptyManager(backupTestRemoteClient{credentials: make(chan [2]string, 1)})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("manager", manager)
	c.Request = httptest.NewRequest("POST", "/api/update", strings.NewReader(`{
		"token_id": "id", "token": "token",
		"plugins": {
			"directory": "/var/lib/pelican/volumes/some-server/plugins",
			"disabled": [],
			"require_signatures": false
		}
	}`))
	c.Request.Header.Set("Content-Type", "application/json")

	postUpdateConfiguration(c)

	if recorder.Code != 200 {
		t.Fatalf("expected the update to be applied, got status %d: %s", recorder.Code, recorder.Body.String())
	}
	got := config.Get().Plugins
	if got.Directory != "/var/lib/pelican/plugins" || len(got.Disabled) != 1 || !got.RequireSignatures {
		t.Fatalf("a configuration push changed plugin policy: %+v", got)
	}
}
