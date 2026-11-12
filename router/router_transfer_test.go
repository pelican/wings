package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apex/log"
	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/router/tokens"
	wserver "github.com/pelican/wings/server"
)

const transferTestServer = "4d1f6a8e-1c3b-4e5a-9f2d-7b6c5a4e3d21"

// setupFailedTransfer lays out what an incoming transfer writes on this node: the
// server's data directory, two received backups, a backup that was already on the
// node, the install log and the machine-id file.
func setupFailedTransfer(t *testing.T) (dataDir string, received []string, existing, installLog, machineID string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Configuration{AuthenticationToken: "transfer-test-token"}
	cfg.System.BackupDirectory = filepath.Join(root, "backups")
	cfg.System.MachineID.Enable = true
	cfg.System.MachineID.Directory = filepath.Join(root, "machine-id")
	config.Set(cfg)

	write := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dataDir = filepath.Join(root, "volumes", transferTestServer)
	write(filepath.Join(dataDir, "world", "level.dat"))
	backupDir := filepath.Join(cfg.System.BackupDirectory, transferTestServer)
	received = []string{filepath.Join(backupDir, "a.tar.gz"), filepath.Join(backupDir, "b.tar.gz")}
	for _, p := range received {
		write(p)
	}
	existing = filepath.Join(backupDir, "already-here.tar.gz")
	write(existing)
	installLog = filepath.Join(root, "logs", "install", transferTestServer+".log")
	write(installLog)
	machineID = filepath.Join(cfg.System.MachineID.Directory, transferTestServer)
	write(machineID)
	return dataDir, received, existing, installLog, machineID
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestRemoveFailedTransferFiles(t *testing.T) {
	dataDir, received, existing, installLog, machineID := setupFailedTransfer(t)

	removeFailedTransferFiles(log.WithField("test", t.Name()), transferTestServer, dataDir, false, received, installLog)

	for _, p := range append([]string{dataDir, installLog, machineID}, received...) {
		if exists(p) {
			t.Errorf("expected %s to be removed", p)
		}
	}
	if !exists(existing) {
		t.Error("a backup that was on the node before the transfer must be kept")
	}
}

func TestRemoveFailedTransferFiles_RemovesEmptyBackupDirectory(t *testing.T) {
	dataDir, received, existing, installLog, _ := setupFailedTransfer(t)
	if err := os.Remove(existing); err != nil {
		t.Fatal(err)
	}

	removeFailedTransferFiles(log.WithField("test", t.Name()), transferTestServer, dataDir, false, received, installLog)

	if exists(filepath.Dir(received[0])) {
		t.Error("expected the now empty backup directory to be removed")
	}
}

func TestRemoveFailedTransferFiles_KeepsPooledData(t *testing.T) {
	dataDir, received, _, installLog, _ := setupFailedTransfer(t)

	removeFailedTransferFiles(log.WithField("test", t.Name()), transferTestServer, dataDir, true, received, installLog)

	if !exists(filepath.Join(dataDir, "world", "level.dat")) {
		t.Error("server files on a shared storage pool belong to the source and must not be deleted")
	}
	for _, p := range received {
		if exists(p) {
			t.Errorf("expected received backup %s to be removed", p)
		}
	}
}

// readFile returns the contents of p, failing the test if it cannot be read.
func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stagedFiles returns the temporary backup files left in dir.
func stagedFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".transfer-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestStageReceivedBackup_KeepsExistingBackupWhenTransferFails(t *testing.T) {
	dataDir, _, existing, installLog, _ := setupFailedTransfer(t)
	backupDir := filepath.Dir(existing)

	tmp, checksum, err := stageReceivedBackup(backupDir, filepath.Base(existing), strings.NewReader("received"))
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256([]byte("received")); checksum != hex.EncodeToString(sum[:]) {
		t.Errorf("unexpected checksum %q", checksum)
	}
	if got := readFile(t, existing); got != "x" {
		t.Fatalf("the existing backup must not change before the transfer is verified, got %q", got)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(tmp); err != nil {
			t.Fatal(err)
		} else if fi.Mode().Perm() != 0o600 {
			t.Errorf("expected the staged backup to have mode 0600, got %o", fi.Mode().Perm())
		}
	}

	// The transfer fails before the backups are committed.
	removeFailedTransferFiles(log.WithField("test", t.Name()), transferTestServer, dataDir, false, []string{tmp}, installLog)

	if got := readFile(t, existing); got != "x" {
		t.Errorf("a backup that was on the node before the transfer must keep its contents, got %q", got)
	}
	if left := stagedFiles(t, backupDir); len(left) != 0 {
		t.Errorf("expected the staged backup to be removed, found %v", left)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestStageReceivedBackup_FailedStreamLeavesNothing(t *testing.T) {
	dir := t.TempDir()

	tmp, _, err := stageReceivedBackup(dir, "a.tar.gz", io.MultiReader(strings.NewReader("partial"), failingReader{}))
	if err == nil {
		t.Fatal("expected the failed stream to return an error")
	}
	if tmp != "" {
		t.Errorf("expected no staged path on error, got %q", tmp)
	}
	if left := stagedFiles(t, dir); len(left) != 0 {
		t.Errorf("expected no staged files after a failed stream, found %v", left)
	}
}

func TestCommitReceivedBackups(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.tar.gz")
	if err := os.WriteFile(existing, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "new.tar.gz")

	staged := map[string]string{}
	for final, content := range map[string]string{existing: "received", fresh: "new"} {
		tmp, _, err := stageReceivedBackup(dir, filepath.Base(final), strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		staged[final] = tmp
	}

	created, err := commitReceivedBackups(staged)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || created[0] != fresh {
		t.Errorf("expected only %s to be reported as created, got %v", fresh, created)
	}
	if got := readFile(t, existing); got != "received" {
		t.Errorf("expected the existing backup to be replaced by the received copy, got %q", got)
	}
	if got := readFile(t, fresh); got != "new" {
		t.Errorf("expected the new backup to be moved into place, got %q", got)
	}
	if left := stagedFiles(t, dir); len(left) != 0 {
		t.Errorf("expected no staged files after committing, found %v", left)
	}
}

// transferTestClient is a Panel client that returns a minimal configuration for
// the transfer test server.
type transferTestClient struct {
	backupTestRemoteClient
}

func (transferTestClient) GetServerConfiguration(context.Context, string) (remote.ServerConfigurationResponse, error) {
	return remote.ServerConfigurationResponse{
		Settings: json.RawMessage(`{"uuid":"` + transferTestServer + `"}`),
	}, nil
}

// newTransferTestRouter returns a router serving POST /api/transfers with an empty
// server manager, and the node's data directory.
func newTransferTestRouter(t *testing.T, configure func(*config.Configuration)) (*gin.Engine, *wserver.Manager, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	cfg := &config.Configuration{AuthenticationToken: "transfer-test-token"}
	cfg.System.Data = filepath.Join(root, "volumes")
	cfg.System.BackupDirectory = filepath.Join(root, "backups")
	cfg.System.LogDirectory = filepath.Join(root, "logs")
	if configure != nil {
		configure(cfg)
	}
	config.Set(cfg)

	client := transferTestClient{}
	m := wserver.NewEmptyManager(client)
	r := gin.New()
	r.Use(middleware.AttachRequestID(), middleware.CaptureErrors(), middleware.AttachServerManager(m), middleware.AttachApiClient(client))
	r.POST("/api/transfers", postTransfers)
	return r, m, cfg.System.Data
}

// postTransfer sends a transfer request for the test server with a valid token
// and no body.
func postTransfer(t *testing.T, r *gin.Engine, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	payload := tokens.TransferPayload{
		Payload: jwt.Payload{
			Subject:        transferTestServer,
			IssuedAt:       jwt.NumericDate(time.Now().Add(2 * time.Second)),
			ExpirationTime: jwt.NumericDate(time.Now().Add(15 * time.Minute)),
		},
		Scoped: tokens.Scoped{Scope: string(tokens.ServerTransfer)},
	}
	token, err := jwt.Sign(payload, config.GetJwtAlgorithm())
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/transfers", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+string(token))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertUnclaimed fails the test if the test server is still claimed.
func assertUnclaimed(t *testing.T) {
	t.Helper()
	if !incomingClaims.claim(transferTestServer) {
		t.Error("expected the request to release its claim on the server")
		return
	}
	incomingClaims.release(transferTestServer)
}

func TestTransferClaims(t *testing.T) {
	tc := transferClaims{ids: make(map[string]struct{})}
	if !tc.claim("a") {
		t.Fatal("expected a free ID to be claimed")
	}
	if tc.claim("a") {
		t.Fatal("expected a claimed ID to be refused")
	}
	if !tc.claim("b") {
		t.Fatal("claims must be per server")
	}
	tc.release("a")
	if !tc.claim("a") {
		t.Fatal("expected a released ID to be claimable again")
	}
}

func TestTransferClaims_ConcurrentRequestsAdmitOne(t *testing.T) {
	tc := transferClaims{ids: make(map[string]struct{})}
	const n = 32
	results := make(chan bool, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			results <- tc.claim(transferTestServer)
		}()
	}
	close(start)
	admitted := 0
	for i := 0; i < n; i++ {
		if <-results {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("expected exactly one of %d concurrent requests to be admitted, got %d", n, admitted)
	}
}

func TestPostTransfers_RefusedWhileClaimed(t *testing.T) {
	r, m, dataDir := newTransferTestRouter(t, nil)

	if !incomingClaims.claim(transferTestServer) {
		t.Fatal("the test server is already claimed")
	}
	defer incomingClaims.release(transferTestServer)

	if w := postTransfer(t, r, nil); w.Code != http.StatusConflict {
		t.Fatalf("expected 409 while another request holds the server, got %d: %s", w.Code, w.Body.String())
	}
	if _, ok := m.Get(transferTestServer); ok {
		t.Error("no server must be created while another request holds the server")
	}
	if exists(filepath.Join(dataDir, transferTestServer)) {
		t.Error("no data directory must be created while another request holds the server")
	}
}

func TestPostTransfers_RefusesServerAlreadyOnNode(t *testing.T) {
	r, m, dataDir := newTransferTestRouter(t, nil)

	// A server that an earlier transfer moved here successfully.
	s, err := wserver.New(transferTestClient{})
	if err != nil {
		t.Fatal(err)
	}
	s.Config().Uuid = transferTestServer
	m.Add(s)
	live := filepath.Join(dataDir, transferTestServer, "world", "level.dat")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The same token is used again, and the request would then fail (it has no body).
	if w := postTransfer(t, r, nil); w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a server that is already on this node, got %d: %s", w.Code, w.Body.String())
	}
	if got := readFile(t, live); got != "live" {
		t.Errorf("the live server's files must be untouched, got %q", got)
	}
	if got := len(m.Filter(func(match *wserver.Server) bool { return match.ID() == transferTestServer })); got != 1 {
		t.Errorf("expected the live server to stay registered exactly once, found %d", got)
	}
	assertUnclaimed(t)
}

func TestPostTransfers_RefusesExistingDataDirectory(t *testing.T) {
	r, m, dataDir := newTransferTestRouter(t, nil)
	leftover := filepath.Join(dataDir, transferTestServer, "keep.txt")
	if err := os.MkdirAll(filepath.Dir(leftover), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leftover, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if w := postTransfer(t, r, nil); w.Code != http.StatusConflict {
		t.Fatalf("expected 409 when files for the server already exist, got %d: %s", w.Code, w.Body.String())
	}
	if !exists(leftover) {
		t.Error("files that were on the node before the transfer must be kept")
	}
	if _, ok := m.Get(transferTestServer); ok {
		t.Error("no server must be created when files for it already exist")
	}
	assertUnclaimed(t)
}

func TestPostTransfers_PooledTransferAllowsExistingDataDirectory(t *testing.T) {
	r, _, dataDir := newTransferTestRouter(t, func(cfg *config.Configuration) {
		cfg.System.Transfers.StoragePool.Enabled = true
		cfg.System.Transfers.StoragePool.PoolName = "shared"
		// Stops the request while initializing the server, see
		// TestPostTransfers_InitFailureRemovesDataDirectory.
		cfg.System.Quotas.Enabled = true
	})
	shared := filepath.Join(dataDir, transferTestServer, "world", "level.dat")
	if err := os.MkdirAll(filepath.Dir(shared), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := postTransfer(t, r, map[string]string{"X-Storage-Pool": "shared"})
	if w.Code == http.StatusConflict {
		t.Fatalf("a pooled transfer must not be refused because the shared data directory exists: %s", w.Body.String())
	}
	if got := readFile(t, shared); got != "shared" {
		t.Errorf("the shared data directory belongs to the source and must be untouched, got %q", got)
	}
	assertUnclaimed(t)
}

func TestPostTransfers_InitFailureRemovesDataDirectory(t *testing.T) {
	// With quotas enabled on a filesystem without quota support, initializing the
	// server fails after its data directory has been created.
	r, m, dataDir := newTransferTestRouter(t, func(cfg *config.Configuration) {
		cfg.System.Quotas.Enabled = true
	})

	if w := postTransfer(t, r, nil); w.Code < 400 || w.Code == http.StatusConflict {
		t.Fatalf("expected the transfer to fail while initializing the server, got %d: %s", w.Code, w.Body.String())
	}
	if exists(filepath.Join(dataDir, transferTestServer)) {
		t.Error("expected the data directory created while initializing the server to be removed")
	}
	if _, ok := m.Get(transferTestServer); ok {
		t.Error("no server must be registered after a failed initialization")
	}
	assertUnclaimed(t)
}
