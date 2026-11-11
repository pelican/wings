package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

func TestPostTransfers_RefusedWhileFailedTransferIsCleanedUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	config.Set(&config.Configuration{AuthenticationToken: "transfer-test-token"})

	client := backupTestRemoteClient{}
	m := wserver.NewEmptyManager(client)
	r := gin.New()
	r.Use(middleware.AttachRequestID(), middleware.CaptureErrors(), middleware.AttachServerManager(m), middleware.AttachApiClient(client))
	r.POST("/api/transfers", postTransfers)

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

	failedIncomingCleanups.Store(transferTestServer, struct{}{})
	defer failedIncomingCleanups.Delete(transferTestServer)

	req := httptest.NewRequest(http.MethodPost, "/api/transfers", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+string(token))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 while a failed transfer is being cleaned up, got %d: %s", w.Code, w.Body.String())
	}
	if _, ok := m.Get(transferTestServer); ok {
		t.Error("no server must be created while the failed transfer is being cleaned up")
	}
}
