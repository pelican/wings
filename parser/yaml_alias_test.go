package parser

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// Changes that go through the node tree must write merge keys as a plain "<<",
// not "!!merge <<", which parsers such as js-yaml cannot read.
func TestUpdateYaml_MergeKeyWrittenWithoutTag(t *testing.T) {
	src := "# settings\ndefaults: &d\n  port: 1\nworlds:\n  world:\n    <<: *d\n    seed: 1\n  nether:\n    <<: *d\n    seed: 2\n"
	for name, rules := range map[string]string{
		"new key":           `[{"match": "worlds.world.extra", "replace_with": "added"}]`,
		"new nested key":    `[{"match": "new.section.key", "replace_with": "added"}]`,
		"wildcard on merge": `[{"match": "worlds.*.enabled", "replace_with": true}]`,
	} {
		out, changed := updateYamlString(t, rules, src)
		if !changed {
			t.Errorf("%s: expected the document to change", name)
			continue
		}
		if strings.Contains(out, "!!merge") {
			t.Errorf("%s: merge key written with an explicit tag:\n%s", name, out)
		}
		if !strings.Contains(out, "\n    <<: *d\n") {
			t.Errorf("%s: expected the merge key to be kept as \"<<: *d\":\n%s", name, out)
		}
		if !strings.Contains(out, "# settings") {
			t.Errorf("%s: expected comments to be kept:\n%s", name, out)
		}
		got := decodeYamlString(t, out)
		worlds := got["worlds"].(map[string]interface{})
		if port := worlds["nether"].(map[string]interface{})["port"]; port != 1 {
			t.Errorf("%s: merge key no longer merges, nether.port = %v\n%s", name, port, out)
		}
	}
}

// A merge alias back into its own anchor decodes, because the explicit key
// shadows the merged one, but can never be expanded. The update must still
// finish with the right values instead of recursing until the stack overflows.
func TestUpdateYaml_SelfReferencingMergeAlias(t *testing.T) {
	src := "a: &a\n  x: 1\n  y:\n    <<: *a\n    y: 2\n"
	out, changed := updateYamlString(t, `[{"match": "a.x", "replace_with": 2}]`, src)
	if !changed {
		t.Fatal("expected the document to change")
	}
	got := decodeYamlString(t, out)
	want := map[string]interface{}{"x": 2, "y": map[string]interface{}{"x": 1, "y": 2}}
	if !reflect.DeepEqual(got["a"], want) {
		t.Errorf("unexpected a %v\n%s", got["a"], out)
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

// An integer above 2^53 is compared exactly when verifying the output. Through a
// float64 the alias copy's 9007199254740993 rounds to the replacement
// 9007199254740992.0, so the in-place patch that changes it through the alias
// would be accepted.
func TestUpdateYaml_AliasedLargeInteger(t *testing.T) {
	src := "base: &b\n  id: 9007199254740993\ncopy: *b\n"
	out, _ := updateYamlString(t, `[{"match": "base.id", "replace_with": 9007199254740992.0}]`, src)
	got := decodeYamlString(t, out)
	if !reflect.DeepEqual(got["base"], map[string]interface{}{"id": 9007199254740992.0}) {
		t.Errorf("unexpected base %#v\n%s", got["base"], out)
	}
	if !reflect.DeepEqual(got["copy"], map[string]interface{}{"id": 9007199254740993}) {
		t.Errorf("alias user changed: copy = %#v\n%s", got["copy"], out)
	}
}

// aliasFanOutYaml returns a document whose last key, through aliases of
// aliases, expands to 10^3 copies of a list of leaf items. The padding comes
// first so the YAML decoder's alias ratio limit accepts it.
func aliasFanOutYaml(leaf, pad int) string {
	var b strings.Builder
	b.WriteString("pad: [p" + strings.Repeat(", p", pad-1) + "]\n")
	b.WriteString("v: &a0 [x" + strings.Repeat(", x", leaf-1) + "]\n")
	for i := 1; i <= 3; i++ {
		ref := fmt.Sprintf("*a%d", i-1)
		fmt.Fprintf(&b, "l%d: &a%d [%s%s]\n", i, i, ref, strings.Repeat(", "+ref, 9))
	}
	return b.String()
}

// Expanding aliases stops once it has copied maxYamlAliasExpansion nodes, and
// the document is then written from the new values.
func TestUpdateYaml_AliasExpansionLimit(t *testing.T) {
	src := aliasFanOutYaml(100, 2000)

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	budget := maxYamlAliasExpansion
	if expandYamlAliases(doc.Content[0], make(map[*yaml.Node]bool), &budget) {
		t.Fatal("expected the expansion to exceed the limit")
	}

	out, _ := updateYamlString(t, `[{"match": "v.0", "replace_with": "y"}]`, src)
	got := decodeYamlString(t, out)
	if v := got["v"].([]interface{}); v[0] != "y" || len(v) != 100 {
		t.Errorf("unexpected v %v", v[:2])
	}
	if l1 := got["l1"].([]interface{}); len(l1) != 10 || l1[0].([]interface{})[0] != "x" {
		t.Errorf("alias user changed: l1[0][0] = %v", l1[0].([]interface{})[0])
	}
}
