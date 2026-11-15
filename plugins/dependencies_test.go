package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGoModRequires(t *testing.T) {
	const content = `
module github.com/example/thing

go 1.21

require github.com/single/dep v1.0.0

require (
	github.com/block/one v1.2.3
	github.com/block/two v0.4.0 // indirect
)

// require github.com/commented/out v9.9.9
`

	got := map[string]string{}
	for _, req := range parseGoModRequires(content) {
		got[req.Path] = req.Version
	}

	want := map[string]string{
		"github.com/single/dep": "v1.0.0",
		"github.com/block/one":  "v1.2.3",
		"github.com/block/two":  "v0.4.0",
	}

	for path, version := range want {
		if got[path] != version {
			t.Errorf("expected %s %s, got %q", path, version, got[path])
		}
	}
	if _, ok := got["github.com/commented/out"]; ok {
		t.Error("a commented-out require should not be read as a dependency")
	}
	if len(got) != len(want) {
		t.Errorf("expected %d requires, got %d: %v", len(want), len(got), got)
	}
}

func TestEscapeModuleElement(t *testing.T) {
	// The proxy encodes an uppercase letter as "!" plus its lowercase form.
	// Without this, any module with capitals in its path cannot be fetched.
	for _, tc := range []struct{ in, want string }{
		{"github.com/google/uuid", "github.com/google/uuid"},
		{"github.com/Masterminds/semver", "github.com/!masterminds/semver"},
		{"v1.2.3", "v1.2.3"},
	} {
		got, err := escapeModuleElement(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("%s -> %q, want %q", tc.in, got, tc.want)
		}
	}

	if _, err := escapeModuleElement(""); err == nil {
		t.Error("an empty element should be refused")
	}
}

func TestCompareModuleVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int // -1, 0 or 1
	}{
		{"v1.0.0", "v1.0.1", -1},
		{"v1.2.0", "v1.10.0", -1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.0.0", "v1.0.0", 0},
		// A prerelease sorts before the release it leads to, which is what
		// decides the winner when two modules require the same dependency.
		{"v1.0.0-rc1", "v1.0.0", -1},
		{"v1.0.0", "v1.0.0-rc1", 1},
		{"v1.0.0+incompatible", "v1.0.0", 0},
	}

	for _, tc := range cases {
		got := compareModuleVersions(tc.a, tc.b)
		sign := 0
		if got < 0 {
			sign = -1
		} else if got > 0 {
			sign = 1
		}
		if sign != tc.want {
			t.Errorf("compare(%s, %s) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestManifestRefusesInexactDependencyVersion(t *testing.T) {
	bridge := newFakeBridge()
	m, root := newTestManager(t, bridge)

	const manifest = `{
        "id": "loose",
        "name": "Loose",
        "author": "Tests",
        "version": "1.0.0",
        "category": "plugin",
        "package": "loose",
        "dependencies": {"github.com/google/uuid": "latest"},
        "meta": {"status": "not_installed", "load_order": 0}
    }`

	writePlugin(t, root, "loose", manifest, "package loose\n")

	if err := m.Discover(); err != nil {
		t.Fatal(err)
	}

	err := m.Install("loose", true)
	if err == nil {
		t.Fatal("a dependency without an exact version should be refused")
	}
	// A range would mean the same plugin behaving differently on two nodes.
	if !strings.Contains(err.Error(), "exact version") {
		t.Fatalf("the error should explain why, got %q", err)
	}
}

func TestDependenciesRefusedWhenDisabledForTheNode(t *testing.T) {
	bridge := newFakeBridge()
	m, root := newTestManager(t, bridge)
	m.cfg.AllowDependencies = false

	const manifest = `{
        "id": "needsdep",
        "name": "Needs Dep",
        "author": "Tests",
        "version": "1.0.0",
        "category": "plugin",
        "package": "needsdep",
        "dependencies": {"github.com/google/uuid": "v1.6.0"},
        "meta": {"status": "not_installed", "load_order": 0}
    }`

	writePlugin(t, root, "needsdep", manifest, "package needsdep\n")

	if err := m.Discover(); err != nil {
		t.Fatal(err)
	}

	err := m.Install("needsdep", true)
	if err == nil {
		t.Fatal("a plugin declaring dependencies should be refused when the node forbids them")
	}
	if !strings.Contains(err.Error(), "allow_dependencies") {
		t.Fatalf("the error should name the setting, got %q", err)
	}
}

func TestKeepModuleFileSkipsWhatCannotBeImported(t *testing.T) {
	keep := []string{"go.mod", "uuid.go", "sub/pkg.go"}
	drop := []string{
		"README.md",           // not source
		"uuid_test.go",        // a module's own tests import things plugins lack
		"testdata/thing.go",   // never imported
		"vendor/x/y.go",       // would shadow the modules laid out alongside
		".github/workflows/x", // not source
	}

	for _, name := range keep {
		if !keepModuleFile(name) {
			t.Errorf("%s should be kept", name)
		}
	}
	for _, name := range drop {
		if keepModuleFile(name) {
			t.Errorf("%s should be skipped", name)
		}
	}
}

// TestPluginUsesARealThirdPartyModule is the end-to-end proof: a plugin
// declares a module, Wings fetches its source from the proxy with no Go
// toolchain involved, and the interpreted plugin imports and calls it.
//
// It needs the network, so it is skipped unless WINGS_TEST_NETWORK is set.
func TestPluginUsesARealThirdPartyModule(t *testing.T) {
	if os.Getenv("WINGS_TEST_NETWORK") == "" {
		t.Skip("set WINGS_TEST_NETWORK=1 to let this reach the Go module proxy")
	}

	bridge := newFakeBridge()
	server := testServer()
	bridge.addServer(server)

	m, root := newTestManager(t, bridge)

	// semver is a good probe: real, small, widely used, and pure computation,
	// so it needs nothing outside the standard library plugins are given.
	const manifest = `{
        "id": "semverdep",
        "name": "Semver Dep",
        "author": "Tests",
        "version": "1.0.0",
        "category": "plugin",
        "package": "semverdep",
        "dependencies": {"golang.org/x/mod": "v0.19.0"},
        "meta": {"status": "not_installed", "load_order": 0}
    }`

	const source = `
package semverdep

import (
	"golang.org/x/mod/semver"

	"github.com/pelican/wings/plugins/api"
)

type plugin struct{}

func (p *plugin) diagnostics() map[string]string {
	return map[string]string{
		"compare":   itoa(semver.Compare("v1.2.0", "v1.10.0")),
		"canonical": semver.Canonical("v1.2"),
		"major":     semver.Major("v3.4.5"),
	}
}

func itoa(i int) string {
	if i < 0 {
		return "-1"
	}
	if i > 0 {
		return "1"
	}
	return "0"
}

func New() api.Registration {
	p := &plugin{}
	return api.Registration{Diagnostics: p.diagnostics}
}
`

	writePlugin(t, root, "semverdep", manifest, source)

	if err := m.Discover(); err != nil {
		t.Fatal(err)
	}

	if err := m.Install("semverdep", true); err != nil {
		inst, _ := m.Get("semverdep")
		t.Fatalf("Install: %v (%s)", err, inst.Manifest().Meta.StatusMessage)
	}

	inst, _ := m.Get("semverdep")
	if inst.Status() != StatusEnabled {
		t.Fatalf("status is %q: %s", inst.Status(), inst.Manifest().Meta.StatusMessage)
	}

	// The module's source is on disk in a GOPATH layout, not compiled.
	src := filepath.Join(root, "semverdep", depsDirName, "src", "golang.org", "x", "mod", "semver", "semver.go")
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("expected the module's source at %s: %v", src, err)
	}

	// And the plugin actually called into it.
	report := Diagnostics()
	fields, ok := report["semverdep"]
	if !ok {
		t.Fatalf("the plugin should report diagnostics, got %v", report)
	}

	for key, want := range map[string]string{
		"compare":   "-1", // v1.2.0 < v1.10.0, which string compare gets wrong
		"canonical": "v1.2.0",
		"major":     "v3",
	} {
		if fields[key] != want {
			t.Errorf("%s = %q, want %q (the library was not really used)", key, fields[key], want)
		}
	}
}
