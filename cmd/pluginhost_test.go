package cmd

import "testing"

// TestHostMethodsRegistered exercises the registry: package init() across the
// domain files must populate hostMethods without a duplicate-registration panic,
// and the expected methods must be present.
func TestHostMethodsRegistered(t *testing.T) {
	for _, m := range []string{
		"resolveAddress",
		"server.list", "server.get", "server.status", "server.state",
		"server.stats", "server.allocations", "server.logs",
		"server.power", "server.command", "events.publish",
		"fs.list", "fs.read", "fs.diskUsage",
		"fs.write", "fs.mkdir", "fs.delete", "fs.rename", "fs.copy", "fs.chmod",
	} {
		if _, ok := hostMethods[m]; !ok {
			t.Fatalf("expected host method %q to be registered", m)
		}
	}
}

// TestHostScopedMethodsHaveHandlers guards against a registry entry with a nil
// handler.
func TestHostScopedMethodsHaveHandlers(t *testing.T) {
	for name, m := range hostMethods {
		if m.fn == nil {
			t.Fatalf("host method %q has a nil handler", name)
		}
	}
}
