package external

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// TestMain gives the tests a configuration when they run as root (in a
// container): plugin processes then run as system.user, which needs one.
// The other tests start plugins from temporary directories only root can
// enter, so they keep running plugins as root; the process user tests below
// set their own configuration.
func TestMain(m *testing.M) {
	if os.Geteuid() == 0 {
		setTestConfig(func(c *config.Configuration) { c.Plugins.RunAsRoot = true })
	}
	os.Exit(m.Run())
}

func setTestConfig(change func(*config.Configuration)) {
	c, err := config.NewAtPath("test.yml")
	if err != nil {
		panic(err)
	}
	c.AuthenticationToken = "test-token"
	change(c)
	config.Set(c)
}

// nobody is the uid and gid the process user tests run plugins as (nobody
// and nogroup on Debian, which the golang image is).
const nobody = 65534

// reportingPlugin installs a plugin that writes what it sees about itself to
// "report" in its data directory, in a plugins directory nobody can reach,
// and returns its manifest.
func reportingPlugin(t *testing.T) plugin.Manifest {
	t.Helper()
	base := t.TempDir()
	// t.TempDir's directories are 0700.
	for _, d := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(base, "plugins", "who")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `d="$WINGS_PLUGIN_DATA"; { id -u; id -g; id -G; echo "$HOME"; ` +
		`if touch ./written 2>/dev/null; then echo own-writable; else echo own-readonly; fi; } > "$d/report"`
	manifest := "apiVersion: v1\nname: who\ncapability: events\nentrypoint: [\"sh\", \"-c\", " + quoteYAML(script) + "]\n"
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := plugin.LoadManifest(filepath.Join(dir, plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func quoteYAML(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// runOnce starts the plugin once. It exits without answering initialize; it
// has run by then.
func runOnce(m plugin.Manifest) error {
	return NewClient(m, nil).EventListener().OnEvent(context.Background(), plugin.Event{Type: "server.state"})
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: Wings only changes the plugin's user when it runs as root")
	}
}

func TestPluginRunsAsTheSystemUserWhenWingsIsRoot(t *testing.T) {
	requireRoot(t)
	setTestConfig(func(c *config.Configuration) { c.System.User.Uid, c.System.User.Gid = nobody, nobody })
	t.Cleanup(func() { setTestConfig(func(c *config.Configuration) { c.Plugins.RunAsRoot = true }) })
	m := reportingPlugin(t)

	// Data a plugin left owned by root when plugins ran as root.
	if err := os.MkdirAll(filepath.Join(m.DataDir(), "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.DataDir(), "state", "old"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(m.DataDir()), 0o700); err != nil {
		t.Fatal(err)
	}

	_ = runOnce(m)
	b, err := os.ReadFile(filepath.Join(m.DataDir(), "report"))
	if err != nil {
		t.Fatalf("the plugin did not run, or could not write its data directory: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{"65534", "65534", "65534", m.DataDir(), "own-readonly"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the plugin saw uid, gid, groups, HOME, own directory = %q, want %q", got, want)
	}
	for _, p := range []string{m.DataDir(), filepath.Join(m.DataDir(), "state"), filepath.Join(m.DataDir(), "state", "old")} {
		st := statT(t, p)
		if st.Uid != nobody || st.Gid != nobody {
			t.Errorf("%s is owned by %d:%d, want %d:%d", p, st.Uid, st.Gid, nobody, nobody)
		}
	}
	if mode := statT(t, m.DataDir()).Mode & 0o777; mode != 0o700 {
		t.Errorf("data directory mode = %o, want 700", mode)
	}
	if mode := statT(t, filepath.Dir(m.DataDir())).Mode & 0o777; mode != 0o711 {
		t.Errorf(".data mode = %o, want 711 so the plugin can reach its own directory", mode)
	}
}

func TestPluginRunsAsRootWhenTheOperatorOptsIn(t *testing.T) {
	requireRoot(t)
	setTestConfig(func(c *config.Configuration) {
		c.System.User.Uid, c.System.User.Gid = nobody, nobody
		c.Plugins.RunAsRoot = true
	})
	m := reportingPlugin(t)
	// Left owned by the system user from before the operator opted in.
	if err := os.MkdirAll(m.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(m.DataDir(), nobody, nobody); err != nil {
		t.Fatal(err)
	}

	_ = runOnce(m)
	b, err := os.ReadFile(filepath.Join(m.DataDir(), "report"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(string(b), "\n")[0]; got != "0" {
		t.Fatalf("uid = %q, want 0 with plugins.run_as_root", got)
	}
	if st := statT(t, m.DataDir()); st.Uid != 0 {
		t.Fatalf("data directory owned by %d, want root again", st.Uid)
	}
}

func TestPluginDoesNotStartWhenItCanChangeItsOwnFiles(t *testing.T) {
	requireRoot(t)
	setTestConfig(func(c *config.Configuration) { c.System.User.Uid, c.System.User.Gid = nobody, nobody })
	t.Cleanup(func() { setTestConfig(func(c *config.Configuration) { c.Plugins.RunAsRoot = true }) })
	m := reportingPlugin(t)
	if err := os.Chown(filepath.Join(m.Dir(), plugin.ManifestFile), nobody, nobody); err != nil {
		t.Fatal(err)
	}

	err := runOnce(m)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected the plugin to be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.DataDir(), "report")); err == nil {
		t.Fatal("the plugin ran")
	}
}

func TestPluginDoesNotRunAsRootByDefault(t *testing.T) {
	requireRoot(t)
	setTestConfig(func(c *config.Configuration) { c.System.Username = "root" })
	t.Cleanup(func() { setTestConfig(func(c *config.Configuration) { c.Plugins.RunAsRoot = true }) })
	m := reportingPlugin(t)

	err := runOnce(m)
	if err == nil || !strings.Contains(err.Error(), "run_as_root") {
		t.Fatalf("a system.user of root must not run plugins as root, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.DataDir(), "report")); err == nil {
		t.Fatal("the plugin ran")
	}
}

func statT(t *testing.T, path string) *syscall.Stat_t {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t)
}
