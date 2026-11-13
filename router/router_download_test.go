package router

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/router/tokens"
	wserver "github.com/pelican/wings/server"
)

const (
	downloadTestServer = "6a3a4f9e-3f4b-4c5e-9a39-3c5d8b8f1a01"
	downloadTestBackup = "0b6c3a51-2d0e-4c55-8f33-6b1b37f2e7a2"
)

// newDownloadTestRouter returns a router serving /download/backup for a single
// server, and the content of the backup archive it serves.
func newDownloadTestRouter(t *testing.T) (*gin.Engine, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	backupDir := t.TempDir()
	config.Set(&config.Configuration{
		AuthenticationToken: "download-test-token",
		System:              config.SystemConfiguration{BackupDirectory: backupDir},
	})

	content := bytes.Repeat([]byte("0123456789"), 100)
	if err := os.MkdirAll(filepath.Join(backupDir, downloadTestServer), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, downloadTestServer, downloadTestBackup+".tar.gz"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	client := backupTestRemoteClient{}
	s, err := wserver.New(client)
	if err != nil {
		t.Fatal(err)
	}
	s.Config().Uuid = downloadTestServer
	m := wserver.NewEmptyManager(client)
	m.Add(s)

	r := gin.New()
	r.Use(middleware.AttachRequestID(), middleware.CaptureErrors(), middleware.AttachServerManager(m), middleware.AttachApiClient(client))
	r.GET("/download/backup", getDownloadBackup)
	r.HEAD("/download/backup", getDownloadBackup)
	return r, content
}

func signBackupToken(t *testing.T, expires time.Time) string {
	t.Helper()
	// Issued slightly in the future, the denylist rejects tokens issued before
	// this process started and NumericDate truncates to whole seconds.
	payload := tokens.BackupPayload{
		Payload: jwt.Payload{
			IssuedAt:       jwt.NumericDate(time.Now().Add(2 * time.Second)),
			ExpirationTime: jwt.NumericDate(expires),
		},
		ServerUuid: downloadTestServer,
		UserUuid:   "5f0d6a8e-8a51-4a53-a0de-91c1d1e6b0a3",
		BackupUuid: downloadTestBackup,
		UniqueId:   "single-unique-id",
		Scoped:     tokens.Scoped{Scope: string(tokens.BackupDownload)},
	}
	b, err := jwt.Sign(payload, config.GetJwtAlgorithm())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func doDownload(r *gin.Engine, method, token string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/download/backup?token="+token, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGetDownloadBackup_RepeatedRequests(t *testing.T) {
	r, content := newDownloadTestRouter(t)
	token := signBackupToken(t, time.Now().Add(15*time.Minute))

	// The same signed URL must keep working: browsers, antivirus software and
	// download managers re-request it.
	for i := 0; i < 3; i++ {
		w := doDownload(r, http.MethodGet, token, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d: %s", i+1, w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), content) {
			t.Fatalf("request %d: body does not match the backup", i+1)
		}
		if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="`+downloadTestBackup+`.tar.gz"` {
			t.Errorf("unexpected Content-Disposition %q", got)
		}
		if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("unexpected Content-Type %q", got)
		}
	}
}

func TestGetDownloadBackup_Range(t *testing.T) {
	r, content := newDownloadTestRouter(t)
	token := signBackupToken(t, time.Now().Add(15*time.Minute))

	w := doDownload(r, http.MethodGet, token, map[string]string{"Range": "bytes=100-199"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), content[100:200]) {
		t.Errorf("unexpected range body %q", w.Body.String())
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 100-199/1000" {
		t.Errorf("unexpected Content-Range %q", got)
	}

	// Resuming from an offset to the end of the file.
	w = doDownload(r, http.MethodGet, token, map[string]string{"Range": "bytes=900-"})
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), content[900:]) {
		t.Fatalf("expected the last 100 bytes, got %d %q", w.Code, w.Body.String())
	}
}

func TestGetDownloadBackup_Head(t *testing.T) {
	r, _ := newDownloadTestRouter(t)
	token := signBackupToken(t, time.Now().Add(15*time.Minute))

	w := doDownload(r, http.MethodHead, token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Length"); got != "1000" {
		t.Errorf("expected Content-Length 1000, got %q", got)
	}
	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("expected Accept-Ranges bytes, got %q", got)
	}
	if w.Body.Len() != 0 {
		t.Errorf("expected an empty body for HEAD, got %d bytes", w.Body.Len())
	}
}

func TestGetDownloadBackup_RejectsExpiredToken(t *testing.T) {
	r, _ := newDownloadTestRouter(t)
	token := signBackupToken(t, time.Now().Add(-time.Minute))

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if w := doDownload(r, method, token, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401 for an expired token, got %d", method, w.Code)
		}
	}
}

func TestGetDownloadBackup_RejectsInvalidToken(t *testing.T) {
	r, _ := newDownloadTestRouter(t)
	token := signBackupToken(t, time.Now().Add(15*time.Minute))

	for name, bad := range map[string]string{
		"bad signature": token[:len(token)-4] + "AAAA",
		"malformed":     "not-a-jwt",
		"empty":         "",
	} {
		if w := doDownload(r, http.MethodGet, bad, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %d", name, w.Code)
		}
	}
}

func TestParseToken_WrapsJwtErrors(t *testing.T) {
	newDownloadTestRouter(t)
	var payload tokens.BackupPayload
	err := tokens.ParseToken([]byte(signBackupToken(t, time.Now().Add(-time.Minute))), &payload)
	if !errors.Is(err, tokens.ErrInvalidToken) || !errors.Is(err, jwt.ErrExpValidation) {
		t.Fatalf("expected the error to match both ErrInvalidToken and jwt.ErrExpValidation, got %v", err)
	}
	// The message is sent to websocket clients and must stay the jwt error's.
	if err.Error() != jwt.ErrExpValidation.Error() {
		t.Errorf("expected the jwt error message %q, got %q", jwt.ErrExpValidation.Error(), err.Error())
	}
}

func TestGetDownloadBackup_RejectsWrongScope(t *testing.T) {
	r, _ := newDownloadTestRouter(t)
	payload := tokens.BackupPayload{
		Payload: jwt.Payload{
			IssuedAt:       jwt.NumericDate(time.Now().Add(2 * time.Second)),
			ExpirationTime: jwt.NumericDate(time.Now().Add(15 * time.Minute)),
		},
		ServerUuid: downloadTestServer,
		UserUuid:   "5f0d6a8e-8a51-4a53-a0de-91c1d1e6b0a3",
		BackupUuid: downloadTestBackup,
		Scoped:     tokens.Scoped{Scope: string(tokens.FileDownload)},
	}
	b, err := jwt.Sign(payload, config.GetJwtAlgorithm())
	if err != nil {
		t.Fatal(err)
	}

	if w := doDownload(r, http.MethodGet, string(b), nil); w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a token without the backup-download scope, got %d", w.Code)
	}
}
