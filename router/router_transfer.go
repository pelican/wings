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

	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/router/tokens"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/installer"
	"github.com/pelican/wings/server/transfer"
)

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

	// Get or create a new transfer instance for this server.
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	trnsfr := transfer.Incoming().Get(u.String())
	if trnsfr == nil {
		// TODO: should this use the request context?
		trnsfr = transfer.New(c, nil)

		ctx, cancel = context.WithCancel(trnsfr.Context())
		defer cancel()

		i, err := installer.New(ctx, manager, installer.ServerDetails{
			UUID:              u.String(),
			StartOnCompletion: false,
		})
		if err != nil {
			if trnsfr.Server != nil {
				if err := manager.Client().SetTransferStatus(context.Background(), trnsfr.Server.ID(), false); err != nil {
					trnsfr.Log().WithField("status", false).WithError(err).Error("failed to set transfer status")
				}
			} else {
				// No server instance yet, so just log the failure without trying to update status
				// Else this will cause: invalid memory address or nil pointer dereference
				trnsfr.Log().WithError(err).Error("failed to initialize transfer; no server instance created")
			}

			middleware.CaptureAndAbort(c, err)
			return
		}

		i.Server().SetTransferring(true)
		manager.Add(i.Server())

		// We add the transfer to the list of transfers once we have a server instance to use.
		trnsfr.Server = i.Server()
		transfer.Incoming().Add(trnsfr)
	} else {
		ctx, cancel = context.WithCancel(trnsfr.Context())
		defer cancel()
	}

	// Any errors past this point (until the transfer is complete) will abort
	// the transfer.

	successful := false
	defer func(ctx context.Context, trnsfr *transfer.Transfer) {
		// Remove the transfer from the list of incoming transfers.
		transfer.Incoming().Remove(trnsfr)

		if !successful {
			trnsfr.Server.Events().Publish(server.TransferStatusEvent, "failure")
			manager.Remove(func(match *server.Server) bool {
				return match.ID() == trnsfr.Server.ID()
			})
			// The server is no longer managed by this node, release its filesystem root.
			defer func() { _ = trnsfr.Server.Filesystem().Close() }()
		}

		if err := manager.Client().SetTransferStatus(context.Background(), trnsfr.Server.ID(), successful); err != nil {
			// Only delete the files if the transfer actually failed, otherwise we could have
			// unrecoverable data-loss.
			if !successful && err != nil {
				// Delete all extracted files.
				go func(trnsfr *transfer.Transfer) {
					_ = trnsfr.Server.Filesystem().Close()
					if err := os.RemoveAll(trnsfr.Server.Filesystem().Path()); err != nil {
						if !errors.Is(err, os.ErrNotExist) {
							trnsfr.Log().WithError(err).Warn("failed to delete local server files")
						}
					}
				}(trnsfr)
			}

			trnsfr.Log().WithField("status", successful).WithError(err).Error("failed to set transfer status on panel")
			return
		}

		trnsfr.Server.SetTransferring(false)
		trnsfr.Server.Events().Publish(server.TransferStatusEvent, "success")
	}(ctx, trnsfr)

	{
		remotePool := config.Get().System.Transfers.StoragePool
		sourcePool := c.GetHeader("X-Storage-Pool")
		if remotePool.Enabled && remotePool.PoolName != "" && sourcePool != "" && strings.EqualFold(remotePool.PoolName, sourcePool) {
			if err := trnsfr.Server.CreateEnvironment(); err != nil {
				middleware.CaptureAndAbort(c, err)
				return
			}
			successful = true
			c.Status(http.StatusOK)
			return
		}
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
				installLogPath := filepath.Join(installLogDir, trnsfr.Server.ID()+".log")

				// Create the install log file
				installLogFile, err := os.Create(installLogPath)
				if err != nil {
					// Don't fail transfer for install logs, just log and continue
					trnsfr.Log().WithError(err).Warn("failed to create install log file, skipping")
					break
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
				trnsfr.Log().WithField("backup", backupName).Debug("received backup file")

				// Create backup directory if it doesn't exist
				cfg := config.Get()
				backupDir := filepath.Join(cfg.System.BackupDirectory, trnsfr.Server.ID())
				if err := os.MkdirAll(backupDir, 0755); err != nil {
					middleware.CaptureAndAbort(c, fmt.Errorf("failed to create backup directory: %w", err))
					return
				}

				backupPath := filepath.Join(backupDir, backupName)

				// Create the backup file and stream directly to disk
				backupFile, err := os.Create(backupPath)
				if err != nil {
					middleware.CaptureAndAbort(c, fmt.Errorf("failed to create backup file %s: %w", backupPath, err))
					return
				}

				// Stream and calculate checksum simultaneously
				hasher := sha256.New()
				tee := io.TeeReader(p, hasher)

				if _, err := io.Copy(backupFile, tee); err != nil {
					backupFile.Close()
					middleware.CaptureAndAbort(c, fmt.Errorf("failed to stream backup file %s: %w", backupName, err))
					return
				}

				if err := backupFile.Close(); err != nil {
					middleware.CaptureAndAbort(c, fmt.Errorf("failed to close backup file %s: %w", backupName, err))
					return
				}

				// Store the checksum for later verification
				backupChecksumsCalculated[backupName] = hex.EncodeToString(hasher.Sum(nil))

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
