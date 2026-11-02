package router

import (
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/router/middleware"
)

// maxPluginRequestBytes bounds the body handed to a plugin route. The body is
// read into memory before the handler sees it, since interpreted code should
// not be given a live stream off the wire to manage.
const maxPluginRequestBytes = 4 << 20

// pluginRouteHandler answers requests aimed at routes a plugin registered.
//
// Wings mounts two catch-alls at boot rather than a Gin route per plugin route,
// because Gin's router cannot be changed once it is serving. Enabling a plugin
// has to work without restarting the daemon, so the registry is consulted per
// request and matching happens in the plugin manager.
//
// The catch-alls sit behind the same authorization every other endpoint uses,
// so a plugin route is reachable by the Panel with the node token and by nobody
// else. A plugin cannot open an unauthenticated endpoint on the node.
func pluginRouteHandler(serverScoped bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		m := plugins.Active()
		if m == nil || !m.Enabled() {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "This node has no plugin subsystem."})
			return
		}

		pluginID := c.Param("plugin")

		// Gin's wildcard keeps the leading slash, and an empty remainder means
		// the plugin's own root.
		path := c.Param("path")
		if path == "" {
			path = "/"
		}

		inst, route, params, ok := m.FindRoute(pluginID, c.Request.Method, path, serverScoped)
		if !ok {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "No enabled plugin on this node answers that route.",
			})
			return
		}

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxPluginRequestBytes+1))
		if err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}
		if len(body) > maxPluginRequestBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": "The request body is larger than a plugin route will accept.",
			})
			return
		}

		req := api.Request{
			Method:       c.Request.Method,
			Path:         path,
			Query:        c.Request.URL.Query(),
			Header:       c.Request.Header.Clone(),
			Body:         body,
			ServerScoped: serverScoped,
		}

		// Path parameters are handed over as query values rather than as a
		// separate map, so a plugin reads ":id" the same way it reads "?id=".
		if len(params) > 0 {
			if req.Query == nil {
				req.Query = map[string][]string{}
			}
			for k, v := range params {
				req.Query[k] = []string{v}
			}
		}

		if serverScoped {
			// ServerExists has already run, so the server is here and the
			// plugin never sees a request for one that is not.
			req.Server = ExtractServer(c).PluginSnapshot()
		}

		res, ok := invokePluginRoute(inst, route, req)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "The plugin handling that route failed.",
			})
			return
		}

		for k, values := range res.Header {
			for _, v := range values {
				c.Writer.Header().Add(k, v)
			}
		}

		status := res.Status
		if status == 0 {
			status = http.StatusOK
		}

		contentType := res.Header["Content-Type"]
		if len(contentType) == 0 {
			c.Data(status, "application/octet-stream", res.Body)
			return
		}
		c.Data(status, strings.Join(contentType, "; "), res.Body)
	}
}

// invokePluginRoute runs a plugin's handler under the same panic containment
// every other hook gets, so a handler that panics becomes a 500 rather than
// taking the daemon down with it.
func invokePluginRoute(inst *plugins.Instance, route api.Route, req api.Request) (api.Response, bool) {
	var res api.Response

	ok := inst.InvokeRoute(func() {
		res = route.Handler(req)
	})

	return res, ok
}
