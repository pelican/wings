package plugin

import "github.com/gin-gonic/gin"

// Scope selects which of Wings' route groups a plugin route mounts into — and
// therefore which middleware (authentication, server lookup) guards it. Wings
// mounts every plugin route under an "/ext/<plugin>/" prefix so plugin routes
// can never collide with or shadow a core Wings route.
type Scope string

const (
	// ScopePublic mounts with NO authentication, e.g. GET /ext/<plugin>/health.
	// Use it only for endpoints that are safe to expose unauthenticated.
	ScopePublic Scope = "public"
	// ScopeNode mounts behind the Panel Bearer token, e.g.
	// GET /api/ext/<plugin>/... — node-wide, not tied to a server.
	ScopeNode Scope = "node"
	// ScopeServer mounts behind the Bearer token and the server-exists check,
	// e.g. POST /api/servers/:server/ext/<plugin>/... — the common case.
	ScopeServer Scope = "server"
)

// Route is one HTTP endpoint a native plugin adds to Wings' API. Path is
// relative to the plugin's mount prefix (e.g. "/health").
type Route struct {
	Scope   Scope
	Method  string // e.g. http.MethodGet
	Path    string // e.g. "/status"
	Handler gin.HandlerFunc

	// plugin is the owning plugin name; set by RegisterRoutes.
	plugin string
}

// Plugin returns the name of the plugin that owns this route.
func (r Route) Plugin() string { return r.plugin }

// routes holds every native plugin route, populated from plugin init()
// functions via RegisterRoutes. It is read once at startup when the router is
// built, so no locking is required.
var routes []Route

// RegisterRoutes adds native HTTP routes for a plugin. Call it from a provider's
// init(), the same way native event listeners register themselves:
//
//	func init() {
//	    plugin.RegisterRoutes("healthz", plugin.Route{
//	        Scope: plugin.ScopePublic, Method: http.MethodGet, Path: "/health",
//	        Handler: func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) },
//	    })
//	}
func RegisterRoutes(pluginName string, rs ...Route) {
	for _, r := range rs {
		r.plugin = pluginName
		routes = append(routes, r)
	}
}

// Routes returns the registered native plugin routes for the router to mount.
func Routes() []Route { return routes }
