// Package auditlog is an example native plugin: Go code compiled into Wings.
//
// It logs every server state change, exposes GET /api/ext/auditlog/recent
// (behind the node token) with the last changes it saw, and prunes that list
// once a minute. Native plugins register themselves from init(); to ship one,
// add a blank import of its package to plugin/builtin/builtin.go:
//
//	import _ "github.com/pelican/wings/examples/plugins/go-native-auditlog"
//
// Nothing imports this package by default, so it is not part of a normal build.
package auditlog

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/plugin"
)

// Name is the plugin name used for the listener, the route prefix and the task.
const Name = "auditlog"

// keepFor is how long a state change stays in the recent list.
const keepFor = time.Hour

// change is one state change the listener saw.
type change struct {
	Server string    `json:"server"`
	State  string    `json:"state"`
	At     time.Time `json:"at"`
}

// recent holds the state changes seen in the last keepFor.
type recent struct {
	mu      sync.Mutex
	changes []change
}

func (r *recent) add(c change) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = append(r.changes, c)
}

func (r *recent) list() []change {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]change(nil), r.changes...)
}

func (r *recent) prune(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.changes[:0]
	for _, c := range r.changes {
		if now.Sub(c.At) < keepFor {
			kept = append(kept, c)
		}
	}
	r.changes = kept
}

// listener implements plugin.EventListener.
type listener struct{ recent *recent }

func (listener) Name() string     { return Name }
func (listener) Events() []string { return []string{"server.state"} }
func (l listener) OnEvent(_ context.Context, e plugin.Event) error {
	state, _ := e.Data["state"].(string)
	l.recent.add(change{Server: e.Server, State: state, At: time.Now()})
	log.WithField("plugin", Name).WithField("server", e.Server).WithField("state", state).Info("server state changed")
	return nil
}

func init() {
	r := &recent{}

	plugin.RegisterEventListener(listener{recent: r})

	// GET /api/ext/auditlog/recent, behind the node token.
	plugin.RegisterRoutes(Name, plugin.Route{
		Scope:  plugin.ScopeNode,
		Method: http.MethodGet,
		Path:   "/recent",
		Handler: func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"changes": r.list()})
		},
	})

	// Runs every minute on Wings' shared scheduler.
	plugin.RegisterTask(Name, plugin.Task{
		Name:     "prune",
		Interval: time.Minute,
		Run: func(context.Context) error {
			r.prune(time.Now())
			return nil
		},
	})
}
