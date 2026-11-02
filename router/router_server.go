package router

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/pelican/wings/config"
	"github.com/pelican/wings/server/filesystem/quotas"

	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/router/downloader"
	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/router/tokens"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/transfer"
)

// Returns a single server from the collection of servers.
func getServer(c *gin.Context) {
	c.JSON(http.StatusOK, ExtractServer(c).ToAPIResponse())
}

// Returns the logs for a given server instance.
func getServerLogs(c *gin.Context) {
	s := middleware.ExtractServer(c)

	l, _ := strconv.Atoi(c.DefaultQuery("size", "100"))
	if l <= 0 {
		l = 100
	} else if l > 100 {
		l = 100
	}

	out, err := s.ReadLogfile(l)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": out})
}

// getServerInstallLogs reads the installation log file for a server and returns
// the portion of the file after the "Script Output" section header.
// If the header is not found, it returns the entire file content.
// If the file cannot be read, it returns an error response.
func getServerInstallLogs(c *gin.Context) {
	s := middleware.ExtractServer(c)
	ID := s.ID()

	filename := filepath.Join(config.Get().System.LogDirectory, "install", ID+".log")

	content, err := os.ReadFile(filename)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read the log for the installation process."})
		return
	}

	// Try to find the script output section
	parts := strings.SplitN(string(content), "| Script Output\n| ------------------------------\n", 2)
	var output string
	if len(parts) == 2 {
		output = parts[1]
	} else {
		// If header not found, return full file
		output = string(content)
	}

	c.JSON(http.StatusOK, gin.H{"data": output})
}

// Handles a request to control the power state of a server. If the action being passed
// through is invalid a 404 is returned. Otherwise, a HTTP/202 Accepted response is returned
// and the actual power action is run asynchronously so that we don't have to block the
// request until a potentially slow operation completes.
//
// This is done because for the most part the Panel is using websockets to determine when
// things are happening, so there's no reason to sit and wait for a request to finish. We'll
// just see over the socket if something isn't working correctly.
func postServerPower(c *gin.Context) {
	s := ExtractServer(c)

	var data struct {
		Action      server.PowerAction `json:"action"`
		WaitSeconds int                `json:"wait_seconds"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	if !data.Action.IsValid() {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{
			"error": "The power action provided was not valid, should be one of \"stop\", \"start\", \"restart\", \"kill\"",
		})
		return
	}

	// Because we route all of the actual bootup process to a separate thread we need to
	// check the suspension status here, otherwise the user will hit the endpoint and then
	// just sit there wondering why it returns a success but nothing actually happens.
	//
	// We don't really care about any of the other actions at this point, they'll all result
	// in the process being stopped, which should have happened anyways if the server is suspended.
	if (data.Action == server.PowerActionStart || data.Action == server.PowerActionRestart) && s.IsSuspended() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Cannot start or restart a server that is suspended.",
		})
		return
	}

	// Pass the actual heavy processing off to a separate thread to handle so that
	// we can immediately return a response from the server. Some of these actions
	// can take quite some time, especially stopping or restarting.
	go func(s *server.Server) {
		if data.WaitSeconds < 0 || data.WaitSeconds > 300 {
			data.WaitSeconds = 30
		}
		if err := s.HandlePowerAction(data.Action, data.WaitSeconds); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				s.Log().WithField("action", data.Action).WithField("error", err).Warn("could not process server power action")
			} else if errors.Is(err, server.ErrIsRunning) {
				// Do nothing, this isn't something we care about for logging,
			} else {
				s.Log().WithFields(log.Fields{"action": data.Action, "wait_seconds": data.WaitSeconds, "error": err}).
					Error("encountered error processing a server power action in the background")
			}
		}
	}(s)

	c.Status(http.StatusAccepted)
}

// Sends an array of commands to a running server instance.
func postServerCommands(c *gin.Context) {
	s := ExtractServer(c)

	if running, err := s.Environment.IsRunning(c.Request.Context()); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	} else if !running {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
			"error": "Cannot send commands to a stopped server instance.",
		})
		return
	}

	var data struct {
		Commands []string `json:"commands"`
	}
	// BindJSON sends 400 if the request fails, all we need to do is return
	if err := c.BindJSON(&data); err != nil {
		return
	}

	// Offer each command to plugins, which may rewrite it or refuse it. A
	// refusal stops that one command rather than the whole batch, so a plugin
	// blocking "stop" does not also swallow the commands sent alongside it.
	var refused []string

	for _, command := range data.Commands {
		if plugins.HasCommandHooks() {
			rewritten, allow, reason := plugins.Command(api.Command{
				Server:  s.PluginSnapshot(),
				Command: command,
				User:    requestUser(c),
			})
			if !allow {
				s.Log().WithFields(log.Fields{"command": command, "reason": reason}).
					Debug("a plugin refused a console command")
				s.PublishConsoleOutputFromDaemon(reason)
				refused = append(refused, reason)
				continue
			}
			command = rewritten
		}

		if err := s.Environment.SendCommand(command); err != nil {
			s.Log().WithFields(log.Fields{"command": command, "error": err}).Warn("failed to send command to server instance")
		}
	}

	// Report a refusal rather than a silent success, so the Panel can show the
	// user why nothing happened. If some commands did go through, the request
	// still succeeded overall.
	if len(refused) > 0 && len(refused) == len(data.Commands) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": refused[0]})
		return
	}

	c.Status(http.StatusNoContent)
}

// postServerSync will accept a POST request and trigger a re-sync of the given
// server against the Panel. This can be manually triggered when needed by an
// external system, or triggered by the Panel itself when modifications are made
// to the build of a server internally.
func postServerSync(c *gin.Context) {
	s := ExtractServer(c)

	if err := s.Sync(); err != nil {
		middleware.CaptureAndAbort(c, err)
	} else {
		c.Status(http.StatusNoContent)
	}
}

// Performs a server installation in a background thread.
func postServerInstall(c *gin.Context) {
	s := ExtractServer(c)

	go func(s *server.Server) {
		s.Log().Info("syncing server state with remote source before executing installation process")
		if err := s.Sync(); err != nil {
			s.Log().WithField("error", err).Error("failed to sync server state with Panel")
			return
		}

		if err := s.Install(); err != nil {
			s.Log().WithField("error", err).Error("failed to execute server installation process")
		}
	}(s)

	c.Status(http.StatusAccepted)
}

// Reinstalls a server.
func postServerReinstall(c *gin.Context) {
	s := ExtractServer(c)

	if s.ExecutingPowerAction() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Cannot execute server reinstall event while another power action is running.",
		})
		return
	}

	go func(s *server.Server) {
		if err := s.Reinstall(); err != nil {
			s.Log().WithField("error", err).Error("failed to complete server re-install process")
		}
	}(s)

	c.Status(http.StatusAccepted)
}

// Deletes a server from the wings daemon and dissociate its objects.
func deleteServer(c *gin.Context) {
	s := middleware.ExtractServer(c)
	ID := s.ID()

	snapshot := s.PluginSnapshot()

	// Ask plugins before anything is suspended or removed. A transfer deletes
	// the source server once it has been copied, and refusing that would strand
	// the transfer, so a plugin only gets a say when this is a real deletion.
	if !s.IsTransferring() {
		if allow, reason := plugins.GateServerDelete(snapshot); !allow {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": reason})
			return
		}
	}

	// Immediately suspend the server to prevent a user from attempting
	// to start it while this process is running.
	s.Config().SetSuspended(true)

	// Notify all websocket clients that the server is being deleted.
	// This is useful for two reasons, one to tell clients not to bother
	// retrying to connect to the websocket.  And two, for transfers when
	// the server has been successfully transferred to another node, and
	// the client needs to switch to the new node.
	if s.IsTransferring() {
		s.Events().Publish(server.TransferStatusEvent, transfer.StatusCompleted)

		// The source server is deleted once a transfer has copied it, so this
		// is where a transfer actually finishes from this node's point of view.
		plugins.Lifecycle(api.Lifecycle{
			Server: snapshot,
			Event:  api.ServerTransferComplete,
		})
	}
	s.Events().Publish(server.DeletedEvent, nil)

	plugins.Lifecycle(api.Lifecycle{Server: snapshot, Event: api.ServerDeleted})

	s.CleanupForDestroy()

	// Remove any pending remote file downloads for the server.
	for _, dl := range downloader.ByServer(s.ID()) {
		dl.Cancel()
	}

	// Remove the install log from this server
	filename := filepath.Join(config.Get().System.LogDirectory, "install", ID+".log")
	err := os.Remove(filename)
	if err != nil && !os.IsNotExist(err) {
		log.WithFields(log.Fields{"server_id": ID, "error": err}).Warn("failed to remove server install log during deletion process")
	}

	// Remove all server backups unless config setting is specified
	if config.Get().System.Backups.RemoveBackupsOnServerDelete == true {
		if err := s.RemoveAllServerBackups(); err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}
	}

	// Destroy the environment; in Docker this will handle a running container and
	// forcibly terminate it before removing the container, so we do not need to handle
	// that here.
	if err := s.Environment.Destroy(); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Once the environment is terminated, remove the server files from the system. This is
	// done in a separate process since failure is not the end of the world and can be
	// manually cleaned up after the fact.
	//
	// In addition, servers with large amounts of files can take some time to finish deleting,
	// so we don't want to block the HTTP call while waiting on this.
	//
	// Only skip file removal when:
	//   1. shared storage pooling is explicitly enabled,
	//   2. the pool has a name configured, and
	//   3. this server is actively being transferred.
	//
	// This avoids preserving data for ordinary server deletions.
	pool := config.Get().System.Transfers.StoragePool
	skipFileRemoval := pool.Enabled && pool.PoolName != "" && s.IsTransferring()
	if !skipFileRemoval {
		go func(s *server.Server) {
			fs := s.Filesystem()
			p := fs.Path()
			_ = fs.UnixFS().Close()
			if err := os.RemoveAll(p); err != nil {
				log.WithFields(log.Fields{"path": p, "error": err}).
					Warn("failed to remove server files during deletion process")
			}

			if config.Get().System.Quotas.Enabled {
				if err = quotas.DelQuota(s.Config().Uuid); err != nil {
					log.WithFields(log.Fields{"server_id": s.Config().Pid, "error": err}).
						Warn("failed to remove quota during deletion process")
				}
			}
		}(s)
	}

	// remove hanging machine-id file for the server when removing
	go func(s *server.Server) {
		if err := os.Remove(filepath.Join(config.Get().System.MachineID.Directory, s.ID())); err != nil {
			log.WithFields(log.Fields{"server_id": s.ID(), "error": err}).
				Warn("failed to remove machine-id file for server")
		}
	}(s)

	middleware.ExtractManager(c).Remove(func(server *server.Server) bool {
		return server.ID() == s.ID()
	})

	c.Status(http.StatusNoContent)
}

// Adds any of the JTIs passed through in the body to the deny list for the websocket
// preventing any JWT generated before the current time from being used to connect to
// the socket or send along commands.
//
// deprecated: prefer /api/deauthorize-user
func postServerDenyWSTokens(c *gin.Context) {
	var data struct {
		JTIs []string `json:"jtis"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	for _, jti := range data.JTIs {
		tokens.DenyJTI(jti)
	}

	c.Status(http.StatusNoContent)
}

func deleteAllServerBackups(c *gin.Context) {
	s := ExtractServer(c)

	if err := s.RemoveAllServerBackups(); err != nil {
		middleware.CaptureAndAbort(c, err)
	} else {
		c.Status(http.StatusNoContent)
	}
}
