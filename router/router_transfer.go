package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/router/tokens"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/filesystem/quotas"
	"github.com/pelican/wings/server/installer"
	"github.com/pelican/wings/server/transfer"
)

// incomingClaims holds the IDs of servers an incoming transfer request is working
// on, from the moment the request is admitted until a failed transfer has been
// removed again. Only one request per server is admitted at a time.
//
// The source node tells the Panel about a failed transfer on its own, so a retry
// can arrive while the old container and files are still being deleted, and that
// deletion would otherwise remove the retry's copy.
var incomingClaims = transferClaims{ids: make(map[string]struct{})}

type transferClaims struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

// claim reserves id and reports whether it was free.
func (tc *transferClaims) claim(id string) bool {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if _, taken := tc.ids[id]; taken {
		return false
	}
	tc.ids[id] = struct{}{}
	return true
}

// release frees id again.
func (tc *transferClaims) release(id string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	delete(tc.ids, id)
}

// isPooledTransfer reports whether the source and this node share the storage
// pool named in the request. The server's data directory then belongs to the
// source as well, and already exists on this node.
func isPooledTransfer(c *gin.Context) bool {
	remotePool := config.Get().System.Transfers.StoragePool
	sourcePool := c.GetHeader("X-Storage-Pool")
	return remotePool.Enabled && remotePool.PoolName != "" && sourcePool != "" && strings.EqualFold(remotePool.PoolName, sourcePool)
}

// postTransfers .
func postTransfers(c *gin.Context) {
	auth := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
	if len(auth) != 2 || auth[0] != "Bearer" {
		c.Header("WWW-Authenticate", "Bearer")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "The required authorization headers were not present in the request.",
		})
		return
	}

	token := tokens.TransferPayload{}
	if err := tokens.ParseToken([]byte(auth[1]), &token); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	if !token.HasScope(tokens.ServerTransfer) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Forbidden."})
		return
	}

	manager := middleware.ExtractManager(c)
	u, err := uuid.Parse(token.Subject)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	id := u.String()
	if !incomingClaims.claim(id) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "A transfer of this server is already running on this node, or a failed one is still being cleaned up.",
		})
		return
	}
	// The claim is released once this request is done, including the cleanup of a
	// failed transfer. A cleanup that could not remove everything keeps it.
	keepClaim := false
	defer func() {
		if !keepClaim {
			incomingClaims.release(id)
		}
	}()

	pooled := isPooledTransfer(c)

	// A transfer must never touch a server this node already has. Transfer tokens
	// can be used until they expire, and a repeated request would otherwise extract
	// over the live server and, if it failed, delete it. Refusing an existing data
	// directory also means that a failed transfer only removes what it created.
	if _, exists := manager.Get(id); exists {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "This server already exists on this node.",
		})
		return
	}
	dataDir := filepath.Join(config.Get().System.Data, id)
	if !pooled {
		if _, err := os.Lstat(dataDir); err == nil {
			log.WithField("server", id).WithField("path", dataDir).
				Warn("refusing incoming transfer: files for this server already exist on this node")
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{
				"error": "Files for this server already exist on this node.",
			})
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			middleware.CaptureAndAbort(c, fmt.Errorf("failed to check for existing server files: %w", err))
			return
		}
	}

	// TODO: should this use the request context?
	trnsfr := transfer.New(c, nil)

	ctx, cancel := context.WithCancel(trnsfr.Context())
	defer cancel()

	i, err := installer.New(ctx, manager, installer.ServerDetails{
		UUID:              id,
		StartOnCompletion: false,
	})
	if err != nil {
		trnsfr.Log().WithError(err).Error("failed to initialize transfer; no server instance created")
		// Initializing the server can fail after its data directory and quota were
		// created. Neither existed before (see above), so remove them again.
		if _, statErr := os.Lstat(dataDir); !pooled && statErr == nil {
			if !removeFailedTransferFiles(trnsfr.Log(), id, dataDir, false, nil, "") {
				keepClaim = true
				trnsfr.Log().Error("could not remove the files of a failed transfer; new transfers of this server are refused until Wings is restarted")
			}
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	i.Server().SetTransferring(true)
	manager.Add(i.Server())

	// We add the transfer to the list of transfers once we have a server instance to use.
	trnsfr.Server = i.Server()
	transfer.Incoming().Add(trnsfr)

	// Any errors past this point (until the transfer is complete) will abort
	// the transfer.

	// Everything this request writes to disk is tracked so that a failed transfer
	// can be removed again. When the server lives on a shared storage pool its data
	// directory belongs to the source as well, and must never be deleted here.
	//
	// Received backups are written to temporary files (stagedBackups maps each
	// final path to its temporary file) and only moved into place once the whole
	// transfer has been verified, so a backup already on this node is never left
	// half overwritten. receivedBackups lists every backup file this transfer has
	// created, temporary or final.
	var (
		successful      bool
		stagedBackups   = make(map[string]string)
		receivedBackups []string
		installLogPath  string
	)
	defer func(ctx context.Context, trnsfr *transfer.Transfer) {
		// Remove the transfer from the list of incoming transfers.
		transfer.Incoming().Remove(trnsfr)

		if !successful {
			trnsfr.Server.Events().Publish(server.TransferStatusEvent, "failure")
			// Only this request's instance can have this ID, see the checks above.
			manager.Remove(func(match *server.Server) bool {
				return match.ID() == id
			})
			// The source node keeps its copy of the server when a transfer fails, so
			// the transfer can be retried or the server moved by hand. Remove the
			// partial copy from this node; the claim refuses a retry until this is
			// done, and for good if the container or the files could not be removed.
			if !cleanupFailedIncomingTransfer(trnsfr, pooled, receivedBackups, installLogPath) {
				keepClaim = true
				trnsfr.Log().Error("could not remove everything a failed transfer left on this node; new transfers of this server are refused until Wings is restarted")
			}
		}

		if err := manager.Client().SetTransferStatus(context.Background(), trnsfr.Server.ID(), successful); err != nil {
			trnsfr.Log().WithField("status", successful).WithError(err).Error("failed to set transfer status on panel")
			return
		}

		if successful {
			trnsfr.Server.SetTransferring(false)
			trnsfr.Server.Events().Publish(server.TransferStatusEvent, "success")
		}
	}(ctx, trnsfr)

	if pooled {
		if err := trnsfr.Server.CreateEnvironment(); err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}
		successful = true
		c.Status(http.StatusOK)
		return
	}

	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		trnsfr.Log().Debug("failed to parse content type header")
		middleware.CaptureAndAbort(c, err)
		return
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		trnsfr.Log().Debug("invalid content type")
		middleware.CaptureAndAbort(c, fmt.Errorf("invalid content type \"%s\", expected \"multipart/form-data\"", mediaType))
		return
	}

	// Used to read the file and checksum from the request body.
	mr := multipart.NewReader(c.Request.Body, params["boundary"])

	var (
		hasArchive                bool
		archiveChecksum           string
		archiveChecksumReceived   string
		backupChecksumsCalculated = make(map[string]string)
		backupChecksumsReceived   = make(map[string]string)
	)
	// Process multipart form
out:
	for {
		select {
		case <-ctx.Done():
			break out
		default:
			p, err := mr.NextPart()
			if err == io.EOF {
				break out
			}
			if err != nil {
				middleware.CaptureAndAbort(c, err)
				return
			}

			name := p.FormName()

			switch {
			case name == "archive":
				trnsfr.Log().Debug("received archive")
				hasArchive = true

				if _, err := trnsfr.Server.EnsureDataDirectoryExists(); err != nil {
					middleware.CaptureAndAbort(c, err)
					return
				}

				// Calculate checksum while streaming to extraction
				archiveHasher := sha256.New()
				tee := io.TeeReader(p, archiveHasher)

				// Stream directly to extraction while calculating checksum
				if err := trnsfr.Server.Filesystem().ExtractStreamUnsafe(ctx, "/", tee); err != nil {
					middleware.CaptureAndAbort(c, err)
					return
				}

				// Store the CALCULATED checksum for later verification
				archiveChecksum = hex.EncodeToString(archiveHasher.Sum(nil))

				trnsfr.Log().Debug("archive extracted and checksum calculated")

			case strings.HasPrefix(name, "checksum_archive"):
				trnsfr.Log().Debug("received archive checksum")
				checksumData, err := io.ReadAll(p)
				if err != nil {
					middleware.CaptureAndAbort(c, err)
					return
				}
				// Store the RECEIVED checksum for verification
				archiveChecksumReceived = string(checksumData)

			case name == "install_logs":
				trnsfr.Log().Debug("received install logs")

				// Create install log directory if it doesn't exist
				cfg := config.Get()
				installLogDir := filepath.Join(cfg.System.LogDirectory, "install")
				if err := os.MkdirAll(installLogDir, 0755); err != nil {
					// Don't fail transfer for install logs, just log and continue
					trnsfr.Log().WithError(err).Warn("failed to create install log directory, skipping")
					break
				}

				// Use the correct install log path with server UUID
				logPath := filepath.Join(installLogDir, trnsfr.Server.ID()+".log")

				// Create the install log file
				installLogFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
				if err != nil {
					// Don't fail transfer for install logs, just log and continue
					trnsfr.Log().WithError(err).Warn("failed to create install log file, skipping")
					break
				}
				installLogPath = logPath
				// OpenFile only applies the mode to new files, so also tighten a log
				// left by an earlier transfer.
				if err := installLogFile.Chmod(0o640); err != nil {
					trnsfr.Log().WithError(err).Warn("failed to set permissions on install log file")
				}

				// Stream the install logs to file
				if _, err := io.Copy(installLogFile, p); err != nil {
					installLogFile.Close()
					// Don't fail transfer for install logs, just log and continue
					trnsfr.Log().WithError(err).Warn("failed to stream install logs to file, skipping")
					break
				}

				if err := installLogFile.Close(); err != nil {
					// Don't fail transfer for install logs, just log and continue
					trnsfr.Log().WithError(err).Warn("failed to close install log file")
				}

				trnsfr.Log().WithField("path", installLogPath).Debug("install logs saved successfully")

			case strings.HasPrefix(name, "backup_"):
				backupNameUnsafe := strings.TrimPrefix(name, "backup_")
				// Strip all directory components
				backupName := filepath.Base(backupNameUnsafe)
				if backupName == "." || backupName == ".." || backupName == string(filepath.Separator) {
					middleware.CaptureAndAbort(c, fmt.Errorf("invalid backup name %q", backupNameUnsafe))
					return
				}
				trnsfr.Log().WithField("backup", backupName).Debug("received backup file")

				// Create backup directory if it doesn't exist
				cfg := config.Get()
				backupDir := filepath.Join(cfg.System.BackupDirectory, trnsfr.Server.ID())
				// Backups are only readable by Wings, the same as backups created on this
				// node (see backup.LocalBackup.Generate). MkdirAll leaves the mode of an
				// existing directory alone, so tighten one left by an older transfer.
				if err := os.MkdirAll(backupDir, 0o700); err != nil {
					middleware.CaptureAndAbort(c, fmt.Errorf("failed to create backup directory: %w", err))
					return
				}
				if err := os.Chmod(backupDir, 0o700); err != nil {
					trnsfr.Log().WithError(err).Warn("failed to set permissions on backup directory")
				}

				backupPath := filepath.Join(backupDir, backupName)
				tmpPath, checksum, err := stageReceivedBackup(backupDir, backupName, p)
				if err != nil {
					middleware.CaptureAndAbort(c, fmt.Errorf("failed to receive backup file %s: %w", backupName, err))
					return
				}
				receivedBackups = append(receivedBackups, tmpPath)
				if previous, ok := stagedBackups[backupPath]; ok {
					// The same backup was sent twice, keep the last copy.
					_ = os.Remove(previous)
				}
				stagedBackups[backupPath] = tmpPath

				// Store the checksum for later verification
				backupChecksumsCalculated[backupName] = checksum

				trnsfr.Log().WithField("backup", backupName).Debug("backup streamed to disk successfully")

			case strings.HasPrefix(name, "checksum_backup_"):
				backupName := strings.TrimPrefix(name, "checksum_backup_")
				trnsfr.Log().WithField("backup", backupName).Debug("received backup checksum")

				checksumData, err := io.ReadAll(p)
				if err != nil {
					middleware.CaptureAndAbort(c, err)
					return
				}
				backupChecksumsReceived[backupName] = string(checksumData)
			}
		}
	}

	// Verify main archive checksum
	if hasArchive {
		if archiveChecksumReceived == "" {
			middleware.CaptureAndAbort(c, errors.New("archive checksum missing"))
			return
		}

		// Compare the calculated checksum with the received checksum
		if archiveChecksum != archiveChecksumReceived {
			trnsfr.Log().WithFields(log.Fields{
				"expected": archiveChecksumReceived,
				"actual":   archiveChecksum,
			}).Error("archive checksum mismatch")
			middleware.CaptureAndAbort(c, errors.New("archive checksum mismatch"))
			return
		}

		trnsfr.Log().Debug("archive checksum verified")
	}

	// Verify backup checksums
	for backupName, calculatedChecksum := range backupChecksumsCalculated {
		receivedChecksum, exists := backupChecksumsReceived[backupName]
		if !exists {
			middleware.CaptureAndAbort(c, fmt.Errorf("checksum missing for backup %s", backupName))
			return
		}

		if calculatedChecksum != receivedChecksum {
			trnsfr.Log().WithFields(log.Fields{
				"backup":   backupName,
				"expected": receivedChecksum,
				"actual":   calculatedChecksum,
			}).Error("backup checksum mismatch")
			middleware.CaptureAndAbort(c, fmt.Errorf("backup %s checksum mismatch", backupName))
			return
		}

		trnsfr.Log().WithField("backup", backupName).Debug("backup checksum verified")
	}

	if !hasArchive {
		middleware.CaptureAndAbort(c, errors.New("missing archive"))
		return
	}

	// Transfer is almost complete, we just want to ensure the environment is
	// configured correctly. We might want to not fail the transfer at this
	// stage, but we will just to be safe.

	// Ensure the server environment gets configured.
	if err := trnsfr.Server.CreateEnvironment(); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Everything has been verified, so move the received backups into place.
	created, err := commitReceivedBackups(stagedBackups)
	receivedBackups = append(receivedBackups, created...)
	if err != nil {
		middleware.CaptureAndAbort(c, fmt.Errorf("failed to store received backups: %w", err))
		return
	}

	// Changing this causes us to notify the panel about a successful transfer,
	// rather than failing the transfer like we do by default.
	successful = true

	// The rest of the logic for ensuring the server is unlocked and everything
	// is handled in the deferred function above.
	trnsfr.Log().Debug("done!")
}

// deleteTransfer cancels an incoming transfer for a server.
func deleteTransfer(c *gin.Context) {
	s := ExtractServer(c)

	if !s.IsTransferring() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Server is not currently being transferred.",
		})
		return
	}

	trnsfr := transfer.Incoming().Get(s.ID())
	if trnsfr == nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Server is not currently being transferred.",
		})
		return
	}

	trnsfr.Cancel()

	c.Status(http.StatusAccepted)
}

// cleanupFailedIncomingTransfer removes everything a failed incoming transfer left
// on this node: the server's container, its data directory, the backups and the
// install log received as part of the transfer, its quota and its machine-id file.
//
// The source node keeps its own copy when a transfer fails, so nothing here is the
// only copy of the server. The exception is a shared storage pool, where the data
// directory is the source's own data, so it is left alone when pooled is true.
// receivedBackups lists only the backup files the transfer created, so backups
// that were already on this node are kept.
//
// It reports whether the container and the data directory are gone. If either is
// left, a new transfer of the server would reuse it, so the caller must refuse one.
func cleanupFailedIncomingTransfer(trnsfr *transfer.Transfer, pooled bool, receivedBackups []string, installLogPath string) bool {
	s := trnsfr.Server
	if s == nil {
		return true
	}
	l := trnsfr.Log()
	ok := true

	if s.Environment != nil {
		if err := s.Environment.Destroy(); err != nil {
			l.WithError(err).Warn("failed to remove the container of a failed transfer")
			ok = false
		}
	}

	dataDir := ""
	if fs := s.Filesystem(); fs != nil {
		dataDir = fs.Path()
		// Release the filesystem root before its directory is removed.
		if err := fs.Close(); err != nil {
			l.WithError(err).Warn("failed to close the filesystem of a failed transfer")
		}
	}

	if !removeFailedTransferFiles(l, s.ID(), dataDir, pooled, receivedBackups, installLogPath) {
		ok = false
	}
	return ok
}

// stageReceivedBackup writes a backup received from the source to a new temporary
// file in backupDir and returns that file's path and the backup's SHA-256 checksum.
// The file is only readable by Wings. On error nothing is left behind.
//
// A backup with the same name that is already on this node stays untouched until
// commitReceivedBackups moves the temporary file into place.
func stageReceivedBackup(backupDir, name string, r io.Reader) (string, string, error) {
	// CreateTemp creates the file with mode 0600.
	f, err := os.CreateTemp(backupDir, ".transfer-"+name+"-*")
	if err != nil {
		return "", "", err
	}
	hasher := sha256.New()
	_, err = io.Copy(f, io.TeeReader(r, hasher))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", "", err
	}
	return f.Name(), hex.EncodeToString(hasher.Sum(nil)), nil
}

// commitReceivedBackups moves each staged backup (keyed by its final path) into
// place, replacing a backup of the same name that is already on this node, which
// is the same backup. It returns the final paths that did not exist before, which
// a failed transfer must remove again, including when it returns an error.
func commitReceivedBackups(staged map[string]string) ([]string, error) {
	var created []string
	for final, tmp := range staged {
		_, statErr := os.Lstat(final)
		if err := os.Rename(tmp, final); err != nil {
			return created, err
		}
		if statErr != nil {
			created = append(created, final)
		}
	}
	return created, nil
}

// removeFailedTransferFiles deletes the files written for a failed incoming
// transfer of the server with the given ID, and reports whether the data directory
// is gone (always true when pooled). See cleanupFailedIncomingTransfer.
func removeFailedTransferFiles(l *log.Entry, id, dataDir string, pooled bool, receivedBackups []string, installLogPath string) bool {
	cfg := config.Get()
	ok := true

	if !pooled && dataDir != "" {
		if err := os.RemoveAll(dataDir); err != nil {
			l.WithError(err).Warn("failed to delete the server files of a failed transfer")
			ok = false
		}
		if cfg.System.Quotas.Enabled {
			if err := quotas.DelQuota(id); err != nil {
				l.WithError(err).Warn("failed to remove the quota of a failed transfer")
			}
		}
	}

	for _, p := range receivedBackups {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			l.WithField("backup", filepath.Base(p)).WithError(err).Warn("failed to delete a backup received by a failed transfer")
		}
	}
	// Only removed when empty, so backups this node already had are kept.
	if cfg.System.BackupDirectory != "" {
		_ = os.Remove(filepath.Join(cfg.System.BackupDirectory, id))
	}

	if installLogPath != "" {
		if err := os.Remove(installLogPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			l.WithError(err).Warn("failed to delete the install log of a failed transfer")
		}
	}
	if cfg.System.MachineID.Enable && cfg.System.MachineID.Directory != "" {
		if err := os.Remove(filepath.Join(cfg.System.MachineID.Directory, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			l.WithError(err).Warn("failed to delete the machine-id file of a failed transfer")
		}
	}

	l.WithField("pooled", pooled).Info("removed the partial copy of a failed incoming transfer")
	return ok
}
