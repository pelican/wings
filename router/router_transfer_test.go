package router

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apex/log"

	"github.com/pelican/wings/config"
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
