package server

import (
	"io"
	"io/fs"
	"os"
	"sort"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/docker/docker/client"
	"github.com/mholt/archives"

	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server/backup"
	"github.com/pelican/wings/server/filesystem"
)

// Notifies the panel of a backup's state and returns an error if one is encountered
// while performing this action.
func (s *Server) notifyPanelOfBackup(uuid string, ad *backup.ArchiveDetails, successful bool) error {
	if err := s.client.SetBackupStatus(s.Context(), uuid, ad.ToRequest(successful)); err != nil {
		if !remote.IsRequestError(err) {
			s.Log().WithFields(log.Fields{
				"backup": uuid,
				"error":  err,
			}).Error("failed to notify panel of backup status due to wings error")
			return err
		}

		return errors.New(err.Error())
	}

	return nil
}

// ignoreFileNames are the server-wide ignore files checked, in order of
// preference. .pteroignore is still honoured for servers migrated from
// Pterodactyl that have not renamed their ignore file.
var ignoreFileNames = []string{".pelicanignore", ".pteroignore"}

// Get all of the ignored files for a server based on its .pelicanignore file in
// the root, falling back to a legacy .pteroignore file if that does not exist.
func (s *Server) getServerwideIgnoredFiles() (string, error) {
	for _, name := range ignoreFileNames {
		ignored, err := s.readIgnoreFile(name)
		if err == nil {
			return ignored, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

// readIgnoreFile reads and validates the ignore file with the given name in the
// server root. An error matching os.ErrNotExist is returned if it does not exist.
func (s *Server) readIgnoreFile(name string) (string, error) {
	f, st, err := s.Filesystem().File(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if st.Mode()&os.ModeSymlink != 0 || st.Size() > filesystem.MaxIgnoreLength {
		// Don't read a symlinked ignore file, or a file larger than the maximum list size.
		return "", nil
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	if err := filesystem.ValidateIgnore(string(b)); err != nil {
		return "", errors.WrapIf(err, "backup: invalid "+name+" file")
	}
	return string(b), nil
}

// Backup performs a server backup and then emits the event over the server
// websocket. We let the actual backup system handle notifying the panel of the
// status, but that won't emit a websocket event.
func (s *Server) Backup(b backup.BackupInterface) error {
	ignored := b.Ignored()
	if b.Ignored() == "" {
		if i, err := s.getServerwideIgnoredFiles(); err != nil {
			log.WithField("server", s.ID()).WithField("error", err).Warn("failed to get server-wide ignored files")
		} else {
			ignored = i
		}
	}

	ad, err := b.Generate(s.Context(), s.Filesystem(), ignored)
	if err != nil {
		if err := s.notifyPanelOfBackup(b.Identifier(), &backup.ArchiveDetails{}, false); err != nil {
			s.Log().WithFields(log.Fields{
				"backup": b.Identifier(),
				"error":  err,
			}).Warn("failed to notify panel of failed backup state")
		} else {
			s.Log().WithField("backup", b.Identifier()).Info("notified panel of failed backup state")
		}

		s.Events().Publish(BackupCompletedEvent+":"+b.Identifier(), map[string]interface{}{
			"uuid":          b.Identifier(),
			"is_successful": false,
			"checksum":      "",
			"checksum_type": "sha1",
			"file_size":     0,
		})

		return errors.WrapIf(err, "backup: error while generating server backup")
	}

	// Try to notify the panel about the status of this backup. If for some reason this request
	// fails, delete the archive from the daemon and return that error up the chain to the caller.
	if notifyError := s.notifyPanelOfBackup(b.Identifier(), ad, true); notifyError != nil {
		_ = b.Remove()

		s.Log().WithField("error", notifyError).Info("failed to notify panel of successful backup state")
		return err
	} else {
		s.Log().WithField("backup", b.Identifier()).Info("notified panel of successful backup state")
	}

	// Emit an event over the socket so we can update the backup in realtime on
	// the frontend for the server.
	s.Events().Publish(BackupCompletedEvent+":"+b.Identifier(), map[string]interface{}{
		"uuid":          b.Identifier(),
		"is_successful": true,
		"checksum":      ad.Checksum,
		"checksum_type": "sha1",
		"file_size":     ad.Size,
	})

	return nil
}

// RestoreBackup calls the Restore function on the provided backup. Once this
// restoration is completed an event is emitted to the websocket to notify the
// Panel that is has been completed.
//
// In addition to the websocket event an API call is triggered to notify the
// Panel of the new state.
func (s *Server) RestoreBackup(b backup.BackupInterface, reader io.ReadCloser) (err error) {
	s.Config().SetSuspended(true)
	// Local backups will not pass a reader through to this function, so check first
	// to make sure it is a valid reader before trying to close it.
	defer func() {
		s.Config().SetSuspended(false)
		if reader != nil {
			_ = reader.Close()
		}
	}()
	// Send an API call to the Panel as soon as this function is done running so that
	// the Panel is informed of the restoration status of this backup.
	defer func() {
		if rerr := s.client.SendRestorationStatus(s.Context(), b.Identifier(), err == nil); rerr != nil {
			s.Log().WithField("error", rerr).WithField("backup", b.Identifier()).Error("failed to notify Panel of backup restoration status")
		}
	}()

	// Don't try to restore the server until we have completely stopped the running
	// instance, otherwise you'll likely hit all types of write errors due to the
	// server being suspended.
	if s.Environment.State() != environment.ProcessOfflineState {
		if err = s.Environment.WaitForStop(s.Context(), 2*time.Minute, false); err != nil {
			if !client.IsErrNotFound(err) {
				return errors.WrapIf(err, "server/backup: restore: failed to wait for container stop")
			}
		}
	}

	// Attempt to restore the backup to the server by running through each entry
	// in the file one at a time and writing them to the disk.
	s.Log().Debug("starting file writing process for backup restoration")

	// Directory modes and times are applied once everything has been restored. A
	// mode without write permission would otherwise stop the files inside it being
	// written, and writing those files would change the directory's time anyway.
	type restoredDir struct {
		name  string
		mode  fs.FileMode
		mtime time.Time
	}
	var dirs []restoredDir

	err = b.Restore(s.Context(), reader, func(file string, info fs.FileInfo, r io.ReadCloser) error {
		defer r.Close()
		if file == "." {
			return nil
		}

		s.Events().Publish(DaemonMessageEvent, "(restoring): "+file)
		if info.IsDir() {
			if err := s.Filesystem().CreateDirectory(file, "/"); err != nil {
				return errors.WithStack(err)
			}
			// Keep setgid and sticky: shared folders rely on them. Setuid means
			// nothing on a directory and is not restored.
			mode := info.Mode() & (fs.ModePerm | fs.ModeSetgid | fs.ModeSticky)
			dirs = append(dirs, restoredDir{name: file, mode: mode, mtime: info.ModTime()})
			return nil
		} else {
			// Symbolic links have no content, so recreate the link itself.
			if info.Mode()&fs.ModeSymlink != 0 {
				fi, ok := info.(archives.FileInfo)
				if !ok || fi.LinkTarget == "" {
					s.Log().WithField("file", file).Warn("skipping symlink without a target during backup restoration")
					return nil
				}
				return errors.WithStack(s.Filesystem().RestoreSymlink(file, fi.LinkTarget))
			}
			// Named pipes and devices are skipped.
			if !info.Mode().IsRegular() {
				return nil
			}

			if err := s.Filesystem().Write(file, r, info.Size(), info.Mode().Perm()); err != nil {
				return errors.WithStack(err)
			}
		}

		atime := info.ModTime()
		return s.Filesystem().Chtimes(file, atime, atime)
	})
	if err != nil {
		return errors.WithStackIf(err)
	}

	// Deepest directories first, so a parent's mode is set after its children.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].name) > len(dirs[j].name) })
	for _, d := range dirs {
		if err := s.Filesystem().Chmod(d.name, d.mode); err != nil {
			return errors.WithStack(err)
		}
		if err := s.Filesystem().Chtimes(d.name, d.mtime, d.mtime); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}
