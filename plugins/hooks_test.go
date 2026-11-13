package plugins

import (
	"testing"
	"time"

	"github.com/pelican/wings/plugins/api"
)

const hooksManifest = `{
    "id": "hooky",
    "name": "Hooky",
    "author": "Tests",
    "version": "1.0.0",
    "category": "plugin",
    "package": "hooky",
    "meta": {"status": "not_installed", "load_order": 0}
}`

// hooksSource exercises the hooks added for shaping startup, vetoing a crash
// restart, watching resource usage and reporting diagnostics.
const hooksSource = `
package hooky

import (
	"strconv"
	"strings"

	"github.com/pelican/wings/plugins/api"
)

type plugin struct {
	host    api.Host
	samples int
	lastMem uint64
}

func (p *plugin) init(h api.Host) error {
	p.host = h
	return nil
}

func (p *plugin) startup(s api.Server, st api.Startup) api.StartupPatch {
	flags := p.host.Settings().String("flags", "")
	if flags == "" || strings.Contains(st.Command, flags) {
		return api.StartupPatch{}
	}

	return api.StartupPatch{
		Command: strings.Replace(st.Command, "java ", "java "+flags+" ", 1),
		Environment: map[string]string{
			"HOOKY_APPLIED": "1",
		},
	}
}

func (p *plugin) crash(info api.CrashInfo) api.Decision {
	// A server killed for running out of memory comes back. One that exited
	// on its own with a bad code stays down for someone to look at.
	if info.OOMKilled {
		return api.Allow()
	}
	if info.ExitCode != 0 {
		return api.Deny("exit code " + strconv.Itoa(info.ExitCode) + " needs a look")
	}
	return api.Allow()
}

func (p *plugin) stats(s api.Server, st api.Stats) {
	p.samples++
	p.lastMem = st.MemoryBytes
}

func (p *plugin) diagnostics() map[string]string {
	return map[string]string{
		"samples":  strconv.Itoa(p.samples),
		"last_mem": strconv.FormatUint(p.lastMem, 10),
	}
}

func New() api.Registration {
	p := &plugin{}
	return api.Registration{
		Init: p.init,
		Settings: []api.Setting{
			{Key: "flags", Type: api.SettingString, Default: "-XX:+UseG1GC"},
		},
		MutateStartup:      p.startup,
		BeforeCrashRestart: p.crash,
		OnStats:            p.stats,
		Diagnostics:        p.diagnostics,
	}
}
`

func TestMutateStartupRewritesCommandAndEnvironment(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)
	enableTestPlugin(t, m, root, "hooky", hooksManifest, hooksSource)

	if !HasStartupHooks() {
		t.Fatal("the plugin registered a startup hook, so dispatch should report one")
	}

	patch := MutateStartup(server, api.Startup{
		Command:     "java -Xms128M -jar server.jar",
		Environment: map[string]string{"STARTUP": "java -Xms128M -jar server.jar"},
	})

	want := "java -XX:+UseG1GC -Xms128M -jar server.jar"
	if patch.Command != want {
		t.Fatalf("startup command was not rewritten:\n got: %q\nwant: %q", patch.Command, want)
	}
	if patch.Environment["HOOKY_APPLIED"] != "1" {
		t.Fatalf("the plugin's environment change was not applied: %v", patch.Environment)
	}

	// Running again over the already-patched command must not stack the flag,
	// which is what would happen on every boot if the hook were not idempotent.
	again := MutateStartup(server, api.Startup{Command: want})
	if again.Command != "" {
		t.Fatalf("a second pass should have left the command alone, got %q", again.Command)
	}
}

func TestCrashRestartCanBeRefused(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)
	enableTestPlugin(t, m, root, "hooky", hooksManifest, hooksSource)

	// Out of memory: the plugin lets it come back.
	if allow, reason := GateCrashRestart(api.CrashInfo{
		Server: server, ExitCode: 137, OOMKilled: true,
	}); !allow {
		t.Fatalf("an OOM kill should have been allowed to restart, got %q", reason)
	}

	// A non-zero exit on its own: the plugin holds it down.
	allow, reason := GateCrashRestart(api.CrashInfo{
		Server: server, ExitCode: 1,
	})
	if allow {
		t.Fatal("the plugin refused this restart, so it should not be allowed")
	}
	if reason == "" {
		t.Fatal("a refusal must carry a reason, or nobody can tell why the server stayed down")
	}
}

func TestStatsAndDiagnosticsReachThePlugin(t *testing.T) {
	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)
	enableTestPlugin(t, m, root, "hooky", hooksManifest, hooksSource)

	if !HasStatsHooks() {
		t.Fatal("the plugin registered a stats hook, so dispatch should report one")
	}

	Stats(server, api.Stats{MemoryBytes: 512, CPUPercent: 12.5})

	// Observing hooks are delivered without waiting, so the sample may not
	// have reached the plugin yet. Poll its own account of what it has seen
	// rather than sleeping a fixed amount, which would either be flaky or
	// slower than it needs to be.
	var fields map[string]string
	deadline := time.Now().Add(2 * time.Second)
	for {
		report := Diagnostics()

		var ok bool
		fields, ok = report["hooky"]
		if !ok {
			t.Fatalf("the plugin should appear in the diagnostics report, got %v", report)
		}
		if fields["samples"] != "0" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if fields["samples"] != "1" {
		t.Errorf("the plugin should have seen one sample, reported %q", fields["samples"])
	}
	if fields["last_mem"] != "512" {
		t.Errorf("the plugin should have seen the memory figure, reported %q", fields["last_mem"])
	}
}
