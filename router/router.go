package router

import (
	"regexp"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/router/middleware"
	wserver "github.com/pelican/wings/server"
)

var tokenRegex = regexp.MustCompile(`([?|&]token=)([^&]+)($|&)`)

// Configure configures the routing infrastructure for this daemon instance.
func Configure(m *wserver.Manager, client remote.Client) *gin.Engine {
	gin.SetMode("release")

	router := gin.New()
	router.Use(gin.Recovery())
	if err := router.SetTrustedProxies(config.Get().Api.TrustedProxies); err != nil {
		panic(errors.WithStack(err))
		return nil
	}
	router.Use(middleware.AttachRequestID(), middleware.CaptureErrors(), middleware.SetAccessControlHeaders())
	router.Use(middleware.AttachServerManager(m), middleware.AttachApiClient(client))
	// @todo log this into a different file so you can setup IP blocking for abusive requests and such.
	// This should still dump requests in debug mode since it does help with understanding the request
	// lifecycle and quickly seeing what was called leading to the logs. However, it isn't feasible to mix
	// this output in production and still get meaningful logs from it since they'll likely just be a huge
	// spamfest.
	router.Use(gin.LoggerWithFormatter(func(params gin.LogFormatterParams) string {
		log.WithFields(log.Fields{
			"client_ip":  params.ClientIP,
			"status":     params.StatusCode,
			"latency":    params.Latency,
			"request_id": params.Keys["request_id"],
		}).Debugf("%s %s",
			params.Method,
			tokenRegex.ReplaceAllString(params.Path, "$1***$3"))

		return ""
	}))

	// These routes use signed URLs to validate access to the resource being requested.
	router.GET("/download/backup", getDownloadBackup)
	router.GET("/download/file", getDownloadFile)
	router.POST("/upload/file", postServerUploadFiles)

	// This route is special it sits above all the other requests because we are
	// using a JWT to authorize access to it, therefore it needs to be publicly
	// accessible.
	router.GET("/api/servers/:server/ws", middleware.ServerExists(), getServerWebsocket)

	// This request is called by another daemon when a server is going to be transferred out.
	// This request does not need the AuthorizationMiddleware as the panel should never call it
	// and requests are authenticated through a JWT the panel issues to the other daemon.
	router.POST("/api/transfers", postTransfers)

	// All the routes beyond this mount will use an authorization middleware
	// and will not be accessible without the correct Authorization header provided.
	protected := router.Use(middleware.RequireAuthorization())
	protected.POST("/api/update", postUpdateConfiguration)
	protected.GET("/api/system", getSystemInformation)
	protected.GET("/api/diagnostics", getDiagnostics)
	protected.GET("/api/system/docker/disk", getDockerDiskUsage)
	protected.DELETE("/api/system/docker/image/prune", pruneDockerImages)
	protected.GET("/api/system/ips", getSystemIps)
	protected.GET("/api/system/utilization", getSystemUtilization)
	protected.GET("/api/servers", getAllServers)

	// Plugin management. These mirror the Panel's own plugin endpoints, so the
	// admin UI can list, enable, disable and uninstall a node's plugins the
	// same way it does the Panel's.
	pluginRoutes := router.Group("/api/plugins")
	pluginRoutes.Use(middleware.RequireAuthorization())
	{
		pluginRoutes.GET("", getPlugins)
		pluginRoutes.POST("/import", postPluginImport)
		pluginRoutes.POST("/order", postPluginOrder)

		pluginRoutes.GET("/:plugin", getPlugin)
		pluginRoutes.GET("/:plugin/source", getPluginSource)
		pluginRoutes.PUT("/:plugin/settings", putPluginSettings)

		pluginRoutes.POST("/:plugin/install", postPluginInstall)
		pluginRoutes.POST("/:plugin/update", postPluginUpdate)
		pluginRoutes.POST("/:plugin/uninstall", postPluginUninstall)
		pluginRoutes.POST("/:plugin/enable", postPluginEnable)
		pluginRoutes.POST("/:plugin/disable", postPluginDisable)

		// Routes plugins registered for themselves, mounted as a catch-all
		// because Gin cannot be given new routes once it is serving and
		// enabling a plugin must not need a restart.
		//
		// They sit under "/http" rather than directly under the plugin, because
		// Gin refuses a catch-all beside static siblings: "/:plugin/*path"
		// conflicts with "/:plugin/enable" and the rest. The extra segment also
		// keeps the management verbs above reserved, so a plugin registering a
		// route called "enable" cannot shadow one.
		pluginRoutes.Any("/:plugin/http/*path", pluginRouteHandler(false))
	}
	protected.POST("/api/servers", postCreateServer)
	protected.DELETE("/api/transfers/:server", deleteTransfer)
	protected.POST("/api/deauthorize-user", postDeauthorizeUser)

	// These are server specific routes, and require that the request be authorized, and
	// that the server exist on the Daemon.
	server := router.Group("/api/servers/:server")
	server.Use(middleware.RequireAuthorization(), middleware.ServerExists())
	{
		server.GET("", getServer)
		server.DELETE("", deleteServer)

		server.GET("/logs", getServerLogs)
		server.GET("/install-logs", getServerInstallLogs)
		server.POST("/power", postServerPower)
		server.POST("/commands", postServerCommands)
		server.POST("/install", postServerInstall)
		server.POST("/reinstall", postServerReinstall)
		server.POST("/sync", postServerSync)
		server.POST("/ws/deny", postServerDenyWSTokens)

		// This archive request causes the archive to start being created
		// this should only be triggered by the panel.
		server.POST("/transfer", postServerTransfer)
		server.DELETE("/transfer", deleteServerTransfer)

		// Deletes all backups for a server
		server.DELETE("deleteAllBackups", deleteAllServerBackups)

		// Server scoped routes a plugin registered. The server is resolved by
		// the group's middleware, so a plugin never sees a request for a
		// server that is not on this node. The "/http" segment matches the
		// node scoped mount above so a plugin's routes are addressed the same
		// way in both scopes.
		server.Any("/plugins/:plugin/http/*path", pluginRouteHandler(true))

		files := server.Group("/files")
		{
			files.GET("/contents", getServerFileContents)
			files.GET("/list-directory", getServerListDirectory)
			files.PUT("/rename", putServerRenameFiles)
			files.POST("/copy", postServerCopyFile)
			files.POST("/write", postServerWriteFile)
			files.POST("/create-directory", postServerCreateDirectory)
			files.POST("/delete", postServerDeleteFiles)
			files.POST("/compress", postServerCompressFiles)
			files.POST("/decompress", postServerDecompressFiles)
			files.POST("/chmod", postServerChmodFile)
			files.GET("/search", getFilesBySearch)

			files.GET("/pull", middleware.RemoteDownloadEnabled(), getServerPullingFiles)
			files.POST("/pull", middleware.RemoteDownloadEnabled(), postServerPullRemoteFile)
			files.DELETE("/pull/:download", middleware.RemoteDownloadEnabled(), deleteServerPullRemoteFile)
		}

		backup := server.Group("/backup")
		{
			backup.POST("", postServerBackup)
			backup.POST("/:backup/restore", postServerRestoreBackup)
			backup.DELETE("/:backup", deleteServerBackup)
		}
	}

	return router
}
