package router

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/events"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/router/tokens"
	wserver "github.com/pelican/wings/server"
)

// existingTransferClient records the transfer results reported to the Panel.
type existingTransferClient struct {
	transferTestClient
	mu       *sync.Mutex
	statuses *[]bool
}

func (c existingTransferClient) SetTransferStatus(_ context.Context, _ string, successful bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.statuses = append(*c.statuses, successful)
	return nil
}

func (c existingTransferClient) reported() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), *c.statuses...)
}

// existingTransferEnvironment is an embedder's environment. Methods the transfer
// must not call panic through the embedded nil interface.
type existingTransferEnvironment struct {
	environment.ProcessEnvironment
	cfg     *environment.Configuration
	bus     *events.Bus
	state   string
	created int
}

func (e *existingTransferEnvironment) Config() *environment.Configuration { return e.cfg }
func (e *existingTransferEnvironment) Events() *events.Bus                { return e.bus }
func (e *existingTransferEnvironment) SetLogCallback(func([]byte))        {}
func (e *existingTransferEnvironment) State() string                      { return e.state }

func (e *existingTransferEnvironment) Create() error {
	e.created++
	return nil
}

// newExistingTransferRouter returns a router serving POST /api/transfers with a
// manager that has the test server, initialized through the embedder's
// environment factory, and that manager's options.
func newExistingTransferRouter(t *testing.T, state string, opts ...wserver.ManagerOption) (*gin.Engine, *wserver.Manager, *wserver.Server, *existingTransferEnvironment, existingTransferClient) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	cfg := &config.Configuration{AuthenticationToken: "transfer-test-token"}
	cfg.System.Data = filepath.Join(root, "volumes")
	cfg.System.BackupDirectory = filepath.Join(root, "backups")
	cfg.System.LogDirectory = filepath.Join(root, "logs")
	config.Set(cfg)

	client := existingTransferClient{mu: &sync.Mutex{}, statuses: &[]bool{}}
	env := &existingTransferEnvironment{bus: events.NewBus(), state: state}
	opts = append(opts, wserver.WithEnvironmentFactory(func(_ *wserver.Server, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
		env.cfg = cfg
		return env, nil
	}))
	m := wserver.NewEmptyManager(client, opts...)
	s, err := m.InitServer(remote.ServerConfigurationResponse{
		Settings:             json.RawMessage(`{"uuid":"` + transferTestServer + `","allocations":{"default":{"ip":"127.0.0.1","port":25565}}}`),
		ProcessConfiguration: &remote.ProcessConfiguration{},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Add(s)

	r := gin.New()
	r.Use(middleware.AttachRequestID(), middleware.CaptureErrors(), middleware.AttachServerManager(m), middleware.AttachApiClient(client))
	r.POST("/api/transfers", postTransfers)
	return r, m, s, env, client
}

// transferBody returns a multipart transfer body with an archive holding name,
// and the checksum the source sends for it.
func transferBody(t *testing.T, name, content, checksum string) (*bytes.Buffer, string) {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg, ModTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if checksum == "" {
		sum := sha256.Sum256(archive.Bytes())
		checksum = hex.EncodeToString(sum[:])
	}

	var body bytes.Buffer
	mp := multipart.NewWriter(&body)
	part, err := mp.CreateFormFile("archive", "archive.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := mp.WriteField("checksum_archive", checksum); err != nil {
		t.Fatal(err)
	}
	if err := mp.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, mp.FormDataContentType()
}

// postTransferBody sends body as a transfer of the test server with a valid token.
func postTransferBody(t *testing.T, r *gin.Engine, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	token, err := jwt.Sign(tokens.TransferPayload{
		Payload: jwt.Payload{
			Subject:        transferTestServer,
			ExpirationTime: jwt.NumericDate(time.Now().Add(15 * time.Minute)),
		},
		Scoped: tokens.Scoped{Scope: string(tokens.ServerTransfer)},
	}, config.GetJwtAlgorithm())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/transfers", body)
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPostTransfers_IntoExistingServer(t *testing.T) {
	r, m, s, env, client := newExistingTransferRouter(t, environment.ProcessOfflineState, wserver.WithTransfersIntoExistingServers())

	body, contentType := transferBody(t, "world/level.dat", "transferred", "")
	if w := postTransferBody(t, r, body, contentType); w.Code != http.StatusOK {
		t.Fatalf("expected the transfer into the existing server to succeed, got %d: %s", w.Code, w.Body.String())
	}

	if got := readFile(t, filepath.Join(config.Get().System.Data, transferTestServer, "world", "level.dat")); got != "transferred" {
		t.Errorf("expected the archive to be extracted into the existing server, got %q", got)
	}
	if got, ok := m.Get(transferTestServer); !ok || got != s {
		t.Error("expected the existing server instance to stay registered")
	}
	if s.IsTransferring() {
		t.Error("expected the server to leave the transferring state")
	}
	if env.created != 1 {
		t.Errorf("expected the environment to be configured once, got %d", env.created)
	}
	if got := client.reported(); len(got) != 1 || !got[0] {
		t.Errorf("expected one successful transfer report, got %v", got)
	}
	assertUnclaimed(t)
}

func TestPostTransfers_IntoExistingServerFailureKeepsServer(t *testing.T) {
	r, m, s, env, client := newExistingTransferRouter(t, environment.ProcessOfflineState, wserver.WithTransfersIntoExistingServers())
	dataDir := filepath.Join(config.Get().System.Data, transferTestServer)
	kept := filepath.Join(dataDir, "keep.txt")
	if err := os.WriteFile(kept, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	body, contentType := transferBody(t, "world/level.dat", "transferred", "not-the-checksum")
	if w := postTransferBody(t, r, body, contentType); w.Code < 400 {
		t.Fatalf("expected a checksum mismatch to fail the transfer, got %d", w.Code)
	}

	if !exists(kept) {
		t.Error("files of the existing server must be kept when the transfer fails")
	}
	if got, ok := m.Get(transferTestServer); !ok || got != s {
		t.Error("the existing server must stay registered when the transfer fails")
	}
	if s.IsTransferring() {
		t.Error("expected the server to leave the transferring state")
	}
	if env.created != 0 {
		t.Errorf("a failed transfer must not configure the environment, got %d calls", env.created)
	}
	if got := client.reported(); len(got) != 1 || got[0] {
		t.Errorf("expected one failed transfer report, got %v", got)
	}
	assertUnclaimed(t)
}

func TestPostTransfers_IntoExistingServerRefusesRunningServer(t *testing.T) {
	r, _, s, _, client := newExistingTransferRouter(t, environment.ProcessRunningState, wserver.WithTransfersIntoExistingServers())

	body, contentType := transferBody(t, "world/level.dat", "transferred", "")
	if w := postTransferBody(t, r, body, contentType); w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a running server, got %d: %s", w.Code, w.Body.String())
	}
	if exists(filepath.Join(config.Get().System.Data, transferTestServer, "world", "level.dat")) {
		t.Error("nothing must be extracted into a running server")
	}
	if s.IsTransferring() {
		t.Error("a refused transfer must not mark the server as transferring")
	}
	if got := client.reported(); len(got) != 0 {
		t.Errorf("a refused transfer must not report a result, got %v", got)
	}
	assertUnclaimed(t)
}

func TestPostTransfers_ExistingServerRefusedWithoutOption(t *testing.T) {
	r, _, _, _, client := newExistingTransferRouter(t, environment.ProcessOfflineState)

	body, contentType := transferBody(t, "world/level.dat", "transferred", "")
	if w := postTransferBody(t, r, body, contentType); w.Code != http.StatusConflict {
		t.Fatalf("expected 409 without WithTransfersIntoExistingServers, got %d: %s", w.Code, w.Body.String())
	}
	if got := client.reported(); len(got) != 0 {
		t.Errorf("a refused transfer must not report a result, got %v", got)
	}
	assertUnclaimed(t)
}
