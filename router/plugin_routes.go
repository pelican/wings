package router

import (
	"path"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/plugin"
)

// mountPluginRoutes attaches every registered native plugin route to the gin
// group for its scope. Each scope group already carries that scope's URL prefix
// and middleware (see Configure), and each route is further namespaced under
// "/<plugin>/<path>", so a plugin route can never collide with or shadow a core
// Wings route. Every mounted route is logged so operators can see exactly what a
// plugin exposed and under which auth scope.
func mountPluginRoutes(scopes map[plugin.Scope]gin.IRoutes) {
	for _, r := range plugin.Routes() {
		group, ok := scopes[r.Scope]
		if !ok {
			log.WithField("plugin", r.Plugin()).
				WithField("scope", string(r.Scope)).
				Warn("skipping plugin route with an unknown scope")
			continue
		}
		relative := path.Join("/", r.Plugin(), r.Path)
		group.Handle(r.Method, relative, r.Handler)
		log.WithFields(log.Fields{
			"plugin": r.Plugin(),
			"scope":  string(r.Scope),
			"method": r.Method,
			"path":   relative,
		}).Info("mounted plugin route")
	}
}
