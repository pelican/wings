package router

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/router/tokens"
	wserver "github.com/pelican/wings/server"
)

// Tests for creating a server from a backup: the backup may belong to a server
// that was deleted, or be stored on another node.

const keptTestServer = "7b4b5a0f-4a5c-4d6f-8b4a-4d6e9c9f2b02"

// setBackupDirectory points the backup directory at a temporary directory for
// the test and returns it.
func setBackupDirectory(t *testing.T) string {
	t.Helper()

	previous := config.Get()
	t.Cleanup(func() {
		config.Set(previous)
	})

	dir := t.TempDir()
	next := *previous
	next.System.BackupDirectory = dir
	config.Set(&next)
	return dir
}

func writeKeptBackup(t *testing.T, dir, serverUuid, backupUuid string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(dir, serverUuid), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, serverUuid, backupUuid+".tar.gz")
	if err := os.WriteFile(p, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func signRestoreToken(t *testing.T, serverUuid string) string {
	t.Helper()

	payload := tokens.BackupPayload{
		Payload: jwt.Payload{
			IssuedAt:       jwt.NumericDate(time.Now().Add(2 * time.Second)),
			ExpirationTime: jwt.NumericDate(time.Now().Add(time.Hour)),
		},
		ServerUuid: serverUuid,
		BackupUuid: downloadTestBackup,
		UniqueId:   "restore" + serverUuid,
		Scoped:     tokens.Scoped{Scope: string(tokens.BackupDownload)},
	}
	b, err := jwt.Sign(payload, config.GetJwtAlgorithm())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A backup kept after its server was deleted can be downloaded by another node
// with a token the Panel issued without a user, and is served as gzip so the
// other node accepts it.
func TestGetDownloadBackup_KeptBackupOfDeletedServer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	config.Set(&config.Configuration{
		AuthenticationToken: "download-test-token",
		System:              config.SystemConfiguration{BackupDirectory: dir},
	})
	writeKeptBackup(t, dir, keptTestServer, downloadTestBackup)

	client := backupTestRemoteClient{}
	r := gin.New()
	r.Use(middleware.AttachRequestID(), middleware.CaptureErrors(), middleware.AttachServerManager(wserver.NewEmptyManager(client)), middleware.AttachApiClient(client))
	r.GET("/download/backup", getDownloadBackup)

	w := doDownload(r, http.MethodGet, signRestoreToken(t, keptTestServer), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "archive" {
		t.Fatalf("unexpected body %q", w.Body.String())
	}
	if !isSupportedBackupRestoreContentType(w.Header().Get("Content-Type")) {
		t.Fatalf("expected a content type a restore accepts, got %q", w.Header().Get("Content-Type"))
	}

	// The server UUID is part of the path, so it has to be a UUID.
	w = doDownload(r, http.MethodGet, signRestoreToken(t, "../"+keptTestServer), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected a token with an invalid server to be rejected, got %d", w.Code)
	}
}

func newKeptBackupDeleteContext(serverUuid, backupUuid string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/backups/"+serverUuid+"/"+backupUuid, nil)
	c.Params = gin.Params{{Key: "server", Value: serverUuid}, {Key: "backup", Value: backupUuid}}
	c.Set("api_client", backupTestRemoteClient{})
	return c, w
}

func TestDeleteKeptBackup(t *testing.T) {
	dir := setBackupDirectory(t)
	p := writeKeptBackup(t, dir, keptTestServer, downloadTestBackup)

	c, w := newKeptBackupDeleteContext(keptTestServer, downloadTestBackup)
	deleteKeptBackup(c)
	if c.Writer.Status() != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", c.Writer.Status(), w.Body.String())
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("expected the backup to be deleted, got %v", err)
	}
	if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
		t.Fatalf("expected the empty server backup directory to be removed, got %v", err)
	}

	// Already gone, the Panel treats this as deleted.
	c, w = newKeptBackupDeleteContext(keptTestServer, downloadTestBackup)
	deleteKeptBackup(c)
	if c.Writer.Status() != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", c.Writer.Status(), w.Body.String())
	}

	c, w = newKeptBackupDeleteContext("..", downloadTestBackup)
	deleteKeptBackup(c)
	if c.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("expected an invalid server to be rejected, got %d: %s", c.Writer.Status(), w.Body.String())
	}
}

func TestPostServerRestoreBackup_SourceServer(t *testing.T) {
	setBackupDirectory(t)
	backupID := "11111111-1111-1111-1111-111111111111"

	c, w, s := newBackupRestoreContext(t, backupTestRemoteClient{}, backupID, `{"adapter":"wings","source_server":"../etc"}`)
	defer s.CtxCancel()
	postServerRestoreBackup(c)
	if c.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("expected an invalid source server to be rejected, got %d: %s", c.Writer.Status(), w.Body.String())
	}

	// Not on this node and no link to download it from.
	c, w, s = newBackupRestoreContext(t, backupTestRemoteClient{}, backupID, fmt.Sprintf(`{"adapter":"wings","source_server":%q}`, keptTestServer))
	defer s.CtxCancel()
	postServerRestoreBackup(c)
	if c.Writer.Status() != http.StatusNotFound {
		t.Fatalf("expected a missing backup to be a 404, got %d: %s", c.Writer.Status(), w.Body.String())
	}
	if s.IsRestoring() {
		t.Fatal("expected the server not to be left restoring")
	}
}

// A local backup this node does not have is downloaded from the node that has
// it, using the link the Panel sent.
func TestPostServerRestoreBackup_LocalBackupOnAnotherNode(t *testing.T) {
	setBackupDirectory(t)
	setBackupRestoreAllowlist(t, []string{"127.0.0.1"})

	hit := make(chan string, 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- r.URL.Path
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer other.Close()

	backupID := "11111111-1111-1111-1111-111111111111"
	body := fmt.Sprintf(`{"adapter":"wings","truncate_directory":true,"source_server":%q,"download_url":%q}`, keptTestServer, other.URL+"/download/backup?token=x")
	c, w, s := newBackupRestoreContext(t, backupTestRemoteClient{}, backupID, body)
	defer s.CtxCancel()

	// The server has no filesystem, so this would panic if the files were
	// truncated before the download failed.
	postServerRestoreBackup(c)

	if c.Writer.Status() != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid response status") {
		t.Fatalf("expected the download to be attempted and fail, got %d: %s", c.Writer.Status(), w.Body.String())
	}
	select {
	case p := <-hit:
		if p != "/download/backup" {
			t.Fatalf("unexpected download path %q", p)
		}
	default:
		t.Fatal("expected the backup to be downloaded from the other node")
	}
}

// Links to another node may use a private address, but loopback stays blocked.
func TestPostServerRestoreBackup_LocalBackupLinkRejectsLoopback(t *testing.T) {
	setBackupDirectory(t)
	setBackupRestoreAllowlist(t, nil)

	backupID := "11111111-1111-1111-1111-111111111111"
	body := fmt.Sprintf(`{"adapter":"wings","source_server":%q,"download_url":"http://127.0.0.1:1/download/backup"}`, keptTestServer)
	c, w, s := newBackupRestoreContext(t, backupTestRemoteClient{}, backupID, body)
	defer s.CtxCancel()

	postServerRestoreBackup(c)
	if c.Writer.Status() != http.StatusBadRequest || !strings.Contains(w.Body.String(), "blocked address") {
		t.Fatalf("expected a loopback link to be blocked, got %d: %s", c.Writer.Status(), w.Body.String())
	}
}

func TestIsBlockedRestoreAddress(t *testing.T) {
	setBackupRestoreAllowlist(t, nil)

	tests := []struct {
		ip           string
		allowPrivate bool
		blocked      bool
	}{
		{"10.0.0.5", false, true},
		{"10.0.0.5", true, false},
		{"192.168.1.10", true, false},
		{"fd00::1", true, false},
		{"100.64.1.1", false, true},
		{"100.64.1.1", true, false},
		{"127.0.0.1", true, true},
		{"169.254.169.254", true, true},
		{"::1", true, true},
		{"0.0.0.0", true, true},
		{"198.18.0.1", true, true},
		{"8.8.8.8", false, false},
		{"8.8.8.8", true, false},
	}
	for _, test := range tests {
		if got := isBlockedRestoreAddress("", net.ParseIP(test.ip), test.allowPrivate); got != test.blocked {
			t.Errorf("%s (allowPrivate=%v): expected blocked to be %v, got %v", test.ip, test.allowPrivate, test.blocked, got)
		}
	}
}

func TestParsePelicanConfig(t *testing.T) {
	tests := map[string]struct {
		expected string
		valid    bool
	}{
		"":                {valid: true},
		"null":            {valid: true},
		` {"a": [1, 2]} `: {expected: `{"a":[1,2]}`, valid: true},
		`[1]`:             {valid: false},
		`"text"`:          {valid: false},
		`{"a":`:           {valid: false},
		`{"a":"` + strings.Repeat("x", 64*1024) + `"}`: {valid: false},
	}
	for raw, test := range tests {
		got, err := parsePelicanConfig([]byte(raw))
		if (err == nil) != test.valid {
			t.Errorf("%.20q: expected valid to be %v, got error %v", raw, test.valid, err)
			continue
		}
		if string(got) != test.expected {
			t.Errorf("%.20q: expected %q, got %q", raw, test.expected, got)
		}
	}
}

func TestPostServerBackupRejectsInvalidPelicanConfig(t *testing.T) {
	backupID := "11111111-1111-1111-1111-111111111111"
	c, w, s := newBackupRestoreContext(t, backupTestRemoteClient{}, backupID, fmt.Sprintf(`{"adapter":"wings","uuid":%q,"pelican_config":[1]}`, backupID))
	defer s.CtxCancel()

	postServerBackup(c)
	if c.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("expected an invalid configuration to be rejected, got %d: %s", c.Writer.Status(), w.Body.String())
	}
}
