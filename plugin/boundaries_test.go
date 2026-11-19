package plugin_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/pelican/wings/"

// boundaries lists, for each plugin module, every Wings package it may depend
// on, directly or not. The plugin system is a set of modules inside one
// binary; this keeps them from growing into each other. In particular a Go
// plugin built with plugin/sdk must never pull in Wings itself.
var boundaries = []struct {
	pkg        string
	wings      []string
	thirdParty bool   // whether non-Wings, non-standard packages are allowed
	why        string // shown when the rule is broken
}{
	{"plugin/contract", nil, false, "plugin authors import it; it must stay standard library only"},
	{"plugin", []string{"config", "system", "plugin/contract", "plugin/jsonrpc"}, true, "the runtime must not depend on installing, running external plugins, or the server layer"},
}

func TestPluginModuleBoundaries(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	for _, b := range boundaries {
		t.Run(b.pkg, func(t *testing.T) {
			out, err := exec.Command("go", "list", "-deps", "-f", "{{.Standard}} {{.ImportPath}}", module+b.pkg).CombinedOutput()
			if err != nil {
				t.Fatalf("go list: %v\n%s", err, out)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				standard, path, _ := strings.Cut(line, " ")
				switch {
				case standard == "true" || path == module+b.pkg:
				case strings.HasPrefix(path, module):
					if !slices.Contains(b.wings, strings.TrimPrefix(path, module)) {
						t.Errorf("%s depends on %s (%s)", b.pkg, path, b.why)
					}
				case !b.thirdParty:
					t.Errorf("%s depends on third-party %s (%s)", b.pkg, path, b.why)
				}
			}
		})
	}
}
