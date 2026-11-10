package parser

import (
	"reflect"
	"strings"
	"testing"
)

// Changing a value inside an anchored node must not change the nodes that use
// it through an alias, as re-marshaling the whole document never did.
func TestUpdateYaml_AnchoredMappingChanged(t *testing.T) {
	src := "# settings\nbase: &base\n  port: 1\n  host: a\ncopy: *base\n"
	out, changed := updateYamlString(t, `[{"match": "base.port", "replace_with": 2}]`, src)
	if !changed {
		t.Fatal("expected the document to change")
	}
	got := decodeYamlString(t, out)
	if !reflect.DeepEqual(got["base"], map[string]interface{}{"port": 2, "host": "a"}) {
		t.Errorf("unexpected base %v\n%s", got["base"], out)
	}
	if !reflect.DeepEqual(got["copy"], map[string]interface{}{"port": 1, "host": "a"}) {
		t.Errorf("alias user changed: copy = %v\n%s", got["copy"], out)
	}
	if !strings.Contains(out, "# settings") {
		t.Errorf("expected comments to be kept:\n%s", out)
	}
}

// Replacing an anchored node must not leave its aliases dangling.
func TestUpdateYaml_AnchoredNodeReplaced(t *testing.T) {
	src := "base: &base\n  port: 1\ncopy: *base\n"
	out, _ := updateYamlString(t, `[{"match": "base", "replace_with": "x"}]`, src)
	got := decodeYamlString(t, out)
	if got["base"] != "x" {
		t.Errorf("unexpected base %v\n%s", got["base"], out)
	}
	if !reflect.DeepEqual(got["copy"], map[string]interface{}{"port": 1}) {
		t.Errorf("unexpected copy %v\n%s", got["copy"], out)
	}
}

func TestUpdateYaml_AnchoredScalarReplaced(t *testing.T) {
	src := "a: &v 1\nb: *v\n"
	out, _ := updateYamlString(t, `[{"match": "a", "replace_with": 2}]`, src)
	got := decodeYamlString(t, out)
	if got["a"] != 2 || got["b"] != 1 {
		t.Errorf("expected a=2 b=1, got %v\n%s", got, out)
	}
}

// Alias users that change along with their anchor still work through the alias.
func TestUpdateYaml_MergeKeyAlias(t *testing.T) {
	src := "defaults: &d\n  port: 1\nserver:\n  <<: *d\n  name: s\n"
	out, _ := updateYamlString(t, `[{"match": "server.name", "replace_with": "t"}]`, src)
	got := decodeYamlString(t, out)
	if !reflect.DeepEqual(got["server"], map[string]interface{}{"port": 1, "name": "t"}) {
		t.Errorf("unexpected server %v\n%s", got["server"], out)
	}
}

// Root keys are decoded as strings, so a key such as "01" must be matched as
// "01" rather than the integer 1.
func TestUpdateYaml_RootKeyNotResolvedAsNumber(t *testing.T) {
	for _, tc := range []struct{ name, src, replace string }{
		{"scalar patch", "01: a\nb: c\n", `[{"match": "01", "replace_with": "x"}]`},
		{"re-encode", "01: a\nb: c\n", `[{"match": "01", "replace_with": "x"}, {"match": "added", "replace_with": "y"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := updateYamlString(t, tc.replace, tc.src)
			if n := strings.Count(out, "01"); n != 1 {
				t.Errorf("expected key 01 once, found %d times:\n%s", n, out)
			}
			got := decodeYamlString(t, out)
			if got["01"] != "x" || got["b"] != "c" {
				t.Errorf("unexpected result %v\n%s", got, out)
			}
		})
	}
}
