package backup

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"emperror.dev/errors"
	"github.com/google/uuid"

	"github.com/pelican/wings/config"
)

// Local backups used to be stored directly in the backup directory as
// "<backup_directory>/<backup_uuid>.tar.gz". They are now stored per server as
// "<backup_directory>/<server_uuid>/<backup_uuid>.tar.gz".
//
// Wings cannot tell which server a legacy archive belongs to on its own, but the
// Panel always provides the server when it downloads, restores, deletes or
// transfers a backup. Legacy archives are therefore moved into the server's
// backup directory the first time they are accessed. Without this they could no
// longer be found, so rotation and deletion from the Panel silently left them on
// disk until the backup volume filled up.

// legacyPath returns the location used for this backup by Wings versions that
// stored local backups directly in the backup directory.
func (b *Backup) legacyPath() (string, error) {
	identifier, err := b.normalizedIdentifier()
	if err != nil {
		return "", err
	}
	return path.Join(config.Get().System.BackupDirectory, identifier+".tar.gz"), nil
}

// MigrateLegacy moves this backup from its legacy location in the root of the
// backup directory into the server's backup directory, if it only exists at the
// legacy location. It is a no-op when the backup already exists at its current
// location or does not exist at the legacy one.
func (b *LocalBackup) MigrateLegacy() error {
	if err := b.validateIdentifier(); err != nil {
		return err
	}
	// Without a server the current and legacy locations would be the same.
	if b.ServerUuid == "" {
		return nil
	}

	if _, err := os.Lstat(b.Path()); !errors.Is(err, os.ErrNotExist) {
		// Either the backup is already where it belongs, or the error will be
		// reported by whatever tries to use it next.
		return nil
	}

	legacy, err := b.legacyPath()
	if err != nil {
		return err
	}
	st, err := os.Lstat(legacy)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.Wrap(err, "backup: failed to stat legacy backup location")
	}
	// Only move regular files, never follow or move a symlink or directory.
	if !st.Mode().IsRegular() {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(b.Path()), 0o700); err != nil {
		return errors.Wrap(err, "backup: failed to create server backup directory")
	}
	if err := os.Rename(legacy, b.Path()); err != nil {
		// Another request for the same backup may have moved it between the
		// checks above and the rename. The rename is atomic, so if the source is
		// gone and the backup now exists at its current location, it was moved.
		if errors.Is(err, os.ErrNotExist) {
			if _, statErr := os.Lstat(b.Path()); statErr == nil {
				return nil
			}
		}
		return errors.Wrap(err, "backup: failed to move backup from legacy location")
	}

	b.log().WithField("from", legacy).WithField("to", b.Path()).Info("moved local backup from legacy location into server backup directory")
	return nil
}

// LegacyLocalArchives returns the number and total size of local backups still
// stored in the legacy location, directly in the root of the backup directory.
func LegacyLocalArchives() (count int, size int64, err error) {
	entries, err := os.ReadDir(config.Get().System.BackupDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".tar.gz")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if _, err := uuid.Parse(name); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		count++
		size += info.Size()
	}
	return count, size, nil
}
