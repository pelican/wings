package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/pelican/wings/config"
	"github.com/pelican/wings/internal/progress"
	"github.com/pelican/wings/server/backup"
	"github.com/pelican/wings/server/filesystem"
)

// Archive returns an archive that can be used to stream the contents of the
// contents of a server.
func (t *Transfer) Archive() (*Archive, error) {
	if t.archive == nil {
		// Get the disk usage of the server (used to calculate the progress of the archive process)
		rawSize, err := t.Server.Filesystem().DiskUsage(true)
		if err != nil {
			return nil, fmt.Errorf("transfer: failed to get server disk usage: %w", err)
		}

		a, err := t.Server.Filesystem().NewArchive(".", filesystem.WithProgress(progress.NewProgress(uint64(rawSize))))
		if err != nil {
			return nil, errors.WrapIf(err, "server/transfer: failed to create archive")
		}
		t.archive = &Archive{archive: a, transfer: t}
	}

	return t.archive, nil
}

func (a *Archive) StreamBackups(ctx context.Context, mp *multipart.Writer) error {
	// In theory this can't happen as this function is only called if there is at least 1 backup but just to be sure
	if len(a.transfer.BackupUUIDs) == 0 {
		a.transfer.Log().Debug("no backups specified for transfer")
		return nil
	}

	// Move any of the requested backups still stored in the legacy location into
	// the server's backup directory so they are included in the transfer. If one
	// cannot be moved the transfer fails, rather than completing without a backup
	// the Panel expects to be on the new node.
	for _, id := range a.transfer.BackupUUIDs {
		if err := backup.NewLocal(nil, id, a.transfer.Server.ID(), "").MigrateLegacy(); err != nil {
			return fmt.Errorf("transfer: failed to move backup %s from legacy location: %w", id, err)
		}
	}

	cfg := config.Get()
	backupPath := filepath.Join(cfg.System.BackupDirectory, a.transfer.Server.ID())

	// Check if backup directory exists
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		a.transfer.Log().Debug("no backups found to transfer")
		return nil
	}

	entries, err := os.ReadDir(backupPath)
	if err != nil {
		return err
	}

	// Create a set of backup UUIDs for quick lookup
	backupSet := make(map[string]bool)
	for _, uuid := range a.transfer.BackupUUIDs {
		backupSet[uuid+".tar.gz"] = true // Backup files are stored as UUID.tar.gz
	}

	var backupsToTransfer []os.DirEntry
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tar.gz") {
			if backupSet[entry.Name()] {
				backupsToTransfer = append(backupsToTransfer, entry)
			}
		}
	}

	totalBackups := len(backupsToTransfer)
	if totalBackups == 0 {
		a.transfer.Log().Debug("no matching backup files found")
		return nil
	}

	a.transfer.Log().Infof("Starting transfer of %d backup files", totalBackups)
	a.transfer.SendMessage(fmt.Sprintf("Starting transfer of %d backup files", totalBackups))

	for _, entry := range backupsToTransfer {
		backupFile := filepath.Join(backupPath, entry.Name())

		a.transfer.Log().WithField("backup", entry.Name()).Debug("streaming backup file")

		// Open backup file for reading
		file, err := os.Open(backupFile)
		if err != nil {
			return fmt.Errorf("failed to open backup file %s: %w", backupFile, err)
		}

		// Create hasher for this specific backup
		backupHasher := sha256.New()
		backupTee := io.TeeReader(file, backupHasher)

		// Create form file for the backup
		part, err := mp.CreateFormFile("backup_"+entry.Name(), entry.Name())
		if err != nil {
			file.Close()
			return fmt.Errorf("failed to create form file for backup %s: %w", entry.Name(), err)
		}

		// Stream the backup file
		if _, err := io.Copy(part, backupTee); err != nil {
			file.Close()
			return fmt.Errorf("failed to stream backup file %s: %w", entry.Name(), err)
		}
		file.Close()

		// Write individual backup checksum
		checksumField := "checksum_backup_" + entry.Name()
		if err := mp.WriteField(checksumField, hex.EncodeToString(backupHasher.Sum(nil))); err != nil {
			return fmt.Errorf("failed to write checksum for backup %s: %w", entry.Name(), err)
		}

		// Update progress tracking
		a.backupsStreamed++

		// Progress message
		progressMsg := fmt.Sprintf("Backup %d/%d completed: %s", a.backupsStreamed, totalBackups, entry.Name())
		a.transfer.Log().Info(progressMsg)
		a.transfer.SendMessage(progressMsg)

		a.transfer.Log().WithFields(log.Fields{
			"backup":   entry.Name(),
			"checksum": checksumField,
		}).Debug("backup file streamed with checksum")
	}

	a.transfer.Log().WithField("count", totalBackups).Debug("finished streaming backups")
	return nil
}

// In archive.go - add this method
func (a *Archive) StreamInstallLogs(ctx context.Context, mp *multipart.Writer) error {
	// Look for install logs in the server directory

	installLogPath := filepath.Join(config.Get().System.LogDirectory, "install", a.transfer.Server.ID()+".log")

	// Check if install log file exists
	if _, err := os.Stat(installLogPath); os.IsNotExist(err) {
		a.transfer.Log().Debug("install logs not found, skipping")
		return nil // No error if logs don't exist
	}

	a.transfer.Log().Debug("streaming install logs")

	// Open install log file for reading
	file, err := os.Open(installLogPath)
	if err != nil {
		// Don't fail the transfer if we can't read install logs
		a.transfer.Log().WithError(err).Warn("failed to open install logs, skipping")
		return nil
	}
	defer file.Close()

	// Create form file for the install logs
	part, err := mp.CreateFormFile("install_logs", "install.log")
	if err != nil {
		// Don't fail the transfer if we can't create form file
		a.transfer.Log().WithError(err).Warn("failed to create form file for install logs, skipping")
		return nil
	}

	// Stream the install log file
	if _, err := io.Copy(part, file); err != nil {
		// Don't fail the transfer if we can't stream install logs
		a.transfer.Log().WithError(err).Warn("failed to stream install logs, skipping")
		return nil
	}

	a.transfer.Log().Debug("install logs streamed successfully")
	return nil
}

// Archive represents an archive used to transfer the contents of a server.
type Archive struct {
	archive         *filesystem.Archive
	transfer        *Transfer
	backupsStreamed int
}

// Stream returns a reader that can be used to stream the contents of the archive.
func (a *Archive) Stream(ctx context.Context, w io.Writer) error {
	return a.archive.Stream(ctx, w)
}

// Progress returns the current progress of the archive.
func (a *Archive) Progress() *progress.Progress {
	return a.archive.Progress()
}
