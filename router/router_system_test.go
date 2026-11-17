package router

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/router/tokens"
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
func newUpdateConfigurationContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder, string) {
	t.Helper()
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")

	path := filepath.Join(t.TempDir(), "config.yml")
	cfg, err := config.NewAtPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationTokenId = "old-id"
	cfg.AuthenticationToken = "old-token"
	cfg.System.User.Uid = 988
	cfg.System.User.Gid = 988
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	config.Set(cfg)

	manager := server.NewEmptyManager(backupTestRemoteClient{credentials: make(chan [2]string, 1)})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("manager", manager)
	c.Request = httptest.NewRequest("POST", "/api/update", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder, path
}

// The request from GHSA-8rw4-fw83-3rv7: the Panel must not be able to change the
// user containers run as, or put them on the host network.
func TestPostUpdateConfigurationIgnoresHostSettings(t *testing.T) {
	c, recorder, _ := newUpdateConfigurationContext(t, `{"token_id":"old-id","token":"old-token","system":{"user":{"uid":0,"gid":0,"passwd":{"directory":"/etc"}}},"docker":{"network":{"mode":"host","network_mode":"host"}}}`)
	before := config.Get()

	postUpdateConfiguration(c)

	if recorder.Code != 200 {
		t.Fatalf("expected the update to be applied, got status %d: %s", recorder.Code, recorder.Body.String())
	}
	after := config.Get()
	if after.System.User.Uid != before.System.User.Uid || after.System.User.Gid != before.System.User.Gid {
		t.Fatalf("expected the container user to be unchanged, got %d:%d", after.System.User.Uid, after.System.User.Gid)
	}
	if after.Docker.Network.Mode != before.Docker.Network.Mode {
		t.Fatalf("expected the network mode to be unchanged, got %q", after.Docker.Network.Mode)
	}
	if after.System.User.Passwd.Directory != before.System.User.Passwd.Directory {
		t.Fatalf("expected the passwd directory to be unchanged, got %q", after.System.User.Passwd.Directory)
	}
}

// An update with an invalid value is refused as a whole, and nothing is written.
func TestPostUpdateConfigurationRejectsInvalidValues(t *testing.T) {
	c, recorder, path := newUpdateConfigurationContext(t, `{"token_id":"new-id","token":"new-token","system":{"username":"pelican --uid 0"}}`)
	before := config.Get()

	postUpdateConfiguration(c)

	if recorder.Code != 422 {
		t.Fatalf("expected the update to be rejected, got status %d", recorder.Code)
	}
	after := config.Get()
	if after.System.Username != before.System.Username || after.Token.ID != "old-id" {
		t.Fatalf("expected the configuration to be unchanged, got username %q and token id %q", after.System.Username, after.Token.ID)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected nothing to be written to disk, got %v", err)
	}
}

// Revoking a user from every server must deny the tokens issued to them, not
// only disconnect them.
func TestPostDeauthorizeUserFromAllServersDeniesTokens(t *testing.T) {
	const serverUuid = "7b0c1b8a-5d2e-4f0e-9a3b-2c1d4e5f6a7b"
	const userUuid = "c3d2e1f0-1a2b-4c3d-8e9f-0a1b2c3d4e5f"

	client := backupTestRemoteClient{}
	s, err := server.New(client)
	if err != nil {
		t.Fatal(err)
	}
	s.Config().Uuid = serverUuid
	m := server.NewEmptyManager(client)
	m.Add(s)

	// Issued after this process started, so only the revocation can deny it.
	issued := time.Now().Add(time.Second).Truncate(time.Second)
	token := &tokens.WebsocketPayload{
		Payload:    jwt.Payload{IssuedAt: jwt.NumericDate(issued), JWTID: "deauthorize-test"},
		ServerUUID: serverUuid,
		UserUUID:   userUuid,
	}
	if token.Denylisted() {
		t.Fatal("expected the token to be accepted before the user is revoked")
	}
	time.Sleep(time.Until(issued) + 10*time.Millisecond)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("manager", m)
	c.Request = httptest.NewRequest("POST", "/api/deauthorize-user", strings.NewReader(`{"user":"`+userUuid+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	postDeauthorizeUser(c)

	if c.Writer.Status() != 204 {
		t.Fatalf("expected the user to be revoked, got status %d", c.Writer.Status())
	}
	if !token.Denylisted() {
		t.Fatal("expected tokens issued before the revocation to be denied")
	}
}
