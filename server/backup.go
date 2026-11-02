package server

import (
	"io"
	"io/fs"
	"os"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/docker/docker/client"

	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
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

// Get all of the ignored files for a server based on its .pelicanignore file in the root.
func (s *Server) getServerwideIgnoredFiles() (string, error) {
	f, st, err := s.Filesystem().File(".pelicanignore")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
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
		return "", errors.WrapIf(err, "backup: invalid .pelicanignore file")
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

	snapshot := s.PluginSnapshot()
	request := api.BackupRequest{
		Server:  snapshot,
		UUID:    b.Identifier(),
		Adapter: string(b.Adapter()),
		Ignore:  ignored,
	}

	// Ask plugins before any files are read. A backup is expensive and
	// competes with the running server for disk, so refusing one is a
	// reasonable thing for a plugin to want to do.
	if allow, reason := plugins.GateBackup(request); !allow {
		s.Log().WithFields(log.Fields{"backup": b.Identifier(), "reason": reason}).
			Info("a plugin refused this backup")

		if err := s.notifyPanelOfBackup(b.Identifier(), &backup.ArchiveDetails{}, false); err != nil {
			s.Log().WithField("error", err).Warn("failed to notify panel of refused backup state")
		}

		plugins.BackupCompleted(api.BackupOutcome{
			Server:  snapshot,
			UUID:    b.Identifier(),
			Adapter: request.Adapter,
			Error:   reason,
		})

		return errors.New(reason)
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

		plugins.BackupCompleted(api.BackupOutcome{
			Server:  snapshot,
			UUID:    b.Identifier(),
			Adapter: request.Adapter,
			Error:   err.Error(),
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

	plugins.BackupCompleted(api.BackupOutcome{
		Server:     snapshot,
		UUID:       b.Identifier(),
		Adapter:    request.Adapter,
		Successful: true,
		SizeBytes:  ad.Size,
		Checksum:   ad.Checksum,
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

	// Reported here rather than at each caller, because local, S3 and plugin
	// restores all come through this one function, and a plugin should hear
	// about a restore whichever adapter performed it.
	defer func() {
		outcome := api.BackupOutcome{
			Server:     s.PluginSnapshot(),
			UUID:       b.Identifier(),
			Adapter:    string(b.Adapter()),
			Successful: err == nil,
		}
		if err != nil {
			outcome.Error = err.Error()
		}

		plugins.Lifecycle(api.Lifecycle{
			Server: outcome.Server,
			Event:  api.ServerBackupRestored,
			Error:  outcome.Error,
		})
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
	err = b.Restore(s.Context(), reader, func(file string, info fs.FileInfo, r io.ReadCloser) error {
		defer r.Close()
		s.Events().Publish(DaemonMessageEvent, "(restoring): "+file)
		// TODO: since this will be called a lot, it may be worth adding an optimized
		// Write with Chtimes method to the UnixFS that is able to re-use the
		// same dirfd and file name.
		if err := s.Filesystem().Write(file, r, info.Size(), info.Mode()); err != nil {
			return err
		}
		atime := info.ModTime()
		return s.Filesystem().Chtimes(file, atime, atime)
	})

	return errors.WithStackIf(err)
}
