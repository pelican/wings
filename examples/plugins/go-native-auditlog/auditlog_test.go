package auditlog

import (
	"context"
	"testing"
	"time"

	"github.com/pelican/wings/plugin"
)

// TestRegistersWithWings checks the three registrations a native plugin makes
// from init(), and that the listener and the prune task behave.
func TestRegistersWithWings(t *testing.T) {
	var route, task bool
	for _, r := range plugin.Routes() {
		if r.Plugin() == Name && r.Path == "/recent" && r.Scope == plugin.ScopeNode {
			route = true
		}
	}
	for _, tk := range plugin.Tasks() {
		if tk.Plugin() == Name && tk.Name == "prune" {
			task = true
		}
	}
	if !route || !task {
		t.Fatalf("route registered: %v, task registered: %v", route, task)
	}

	r := &recent{}
	l := listener{recent: r}
	if err := l.OnEvent(context.Background(), plugin.Event{Type: "server.state", Server: "abc", Data: map[string]any{"state": "running"}}); err != nil {
		t.Fatal(err)
	}
	if got := r.list(); len(got) != 1 || got[0].Server != "abc" || got[0].State != "running" {
		t.Fatalf("recent = %+v", got)
	}
	r.prune(time.Now().Add(2 * keepFor))
	if got := r.list(); len(got) != 0 {
		t.Fatalf("prune kept %d old changes", len(got))
	}
}
