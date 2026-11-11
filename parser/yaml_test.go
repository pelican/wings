package parser

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"gopkg.in/yaml.v3"
)

func newYamlConfigurationFile(t *testing.T, replace string) *ConfigurationFile {
	t.Helper()
	var f ConfigurationFile
	if err := json.Unmarshal([]byte(`{"file":"config.yml","parser":"yaml","replace":`+replace+`}`), &f); err != nil {
		t.Fatal(err)
	}
	return &f
}

func updateYamlString(t *testing.T, replace, src string) (string, bool) {
	t.Helper()
	out, changed, err := newYamlConfigurationFile(t, replace).updateYaml([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return string(out), changed
}

func decodeYamlString(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var v map[string]interface{}
	if err := yaml.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("output is not valid YAML: %v\n%s", err, s)
	}
	return v
}

const luckPermsLikeConfig = `####################################################
#              LuckPerms Configuration              #
####################################################

# The name of the server, used for server specific permissions.
server: global

# How the plugin should store data
storage-method: h2

data:
  # Define the address and port for the database.
  address: localhost
  database: 'minecraft'
  port: 3306   # default MySQL port

  # Credentials for the database.
  username: "root"

zeta-option: true
alpha-option: false
snowflake: 1234567890123456789
`

// Comments, blank lines, key order, quoting and untouched values are preserved
// exactly; only the replaced values change (#66).
func TestUpdateYaml_PreservesLayout(t *testing.T) {
	out, changed := updateYamlString(t, `[
		{"match": "server", "replace_with": "lobby"},
		{"match": "data.address", "replace_with": "10.0.0.5"},
		{"match": "data.database", "replace_with": "luckperms"},
		{"match": "data.port", "replace_with": "3307"},
		{"match": "data.username", "replace_with": "pelican"},
		{"match": "zeta-option", "replace_with": "false"}
	]`, luckPermsLikeConfig)

	want := strings.NewReplacer(
		"server: global", "server: lobby",
		"address: localhost", "address: 10.0.0.5",
		"database: 'minecraft'", "database: 'luckperms'",
		"port: 3306   #", "port: 3307   #",
		`username: "root"`, `username: "pelican"`,
		"zeta-option: true", "zeta-option: false",
	).Replace(luckPermsLikeConfig)

	if !changed {
		t.Fatal("expected the document to change")
	}
	if out != want {
		t.Fatalf("unexpected output\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestUpdateYaml_NoChangeLeavesFileUntouched(t *testing.T) {
	out, changed := updateYamlString(t, `[
		{"match": "server", "replace_with": "global"},
		{"match": "data.port", "replace_with": "3306"}
	]`, luckPermsLikeConfig)
	if changed {
		t.Error("expected no change to be reported")
	}
	if out != luckPermsLikeConfig {
		t.Errorf("expected the document to be returned as-is, got\n%s", out)
	}
}

// Values that are not safe as plain scalars are quoted.
func TestUpdateYaml_QuotesUnsafeValues(t *testing.T) {
	src := "# message of the day\nmotd: hello\nport: 25565\n"
	out, _ := updateYamlString(t, `[
		{"match": "motd", "replace_with": "a: b #c"},
		{"match": "port", "replace_with": "25566"}
	]`, src)

	v := decodeYamlString(t, out)
	if v["motd"] != "a: b #c" {
		t.Errorf("unexpected motd %#v in\n%s", v["motd"], out)
	}
	if v["port"] != 25566 {
		t.Errorf("expected port to stay a number, got %#v in\n%s", v["port"], out)
	}
	if !strings.HasPrefix(out, "# message of the day\n") {
		t.Errorf("expected the comment to be kept, got\n%s", out)
	}
}

func TestUpdateYaml_Wildcard(t *testing.T) {
	src := `servers:
  # the lobby
  lobby:
    address: 127.0.0.1:25566
    restricted: false
  survival:
    address: 127.0.0.1:25567 # survival world
    restricted: false
`
	out, _ := updateYamlString(t, `[{"match": "servers.*.address", "replace_with": "0.0.0.0:25565"}]`, src)
	want := strings.ReplaceAll(src, "127.0.0.1:25566", "0.0.0.0:25565")
	want = strings.ReplaceAll(want, "127.0.0.1:25567", "0.0.0.0:25565")
	if out != want {
		t.Fatalf("unexpected output\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestUpdateYaml_IfValue(t *testing.T) {
	src := "host: 127.0.0.1 # bind\nother: 127.0.0.1\nkeep: localhost\n"
	out, _ := updateYamlString(t, `[
		{"match": "host", "if_value": "127.0.0.1", "replace_with": "0.0.0.0"},
		{"match": "keep", "if_value": "127.0.0.1", "replace_with": "0.0.0.0"}
	]`, src)
	if want := "host: 0.0.0.0 # bind\nother: 127.0.0.1\nkeep: localhost\n"; out != want {
		t.Fatalf("unexpected output\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// Adding a key cannot be done in place; the document is re-encoded keeping the
// original key order and comments, and the new key is appended.
func TestUpdateYaml_AddedKeyKeepsOrderAndComments(t *testing.T) {
	src := `# top comment
zeta: 1
# about alpha
alpha:
  b: 2
  a: 1
`
	out, changed := updateYamlString(t, `[{"match": "alpha.new-key", "replace_with": "value"}]`, src)
	if !changed {
		t.Fatal("expected the document to change")
	}
	for _, s := range []string{"# top comment", "# about alpha"} {
		if !strings.Contains(out, s) {
			t.Errorf("expected comment %q to be kept in\n%s", s, out)
		}
	}
	order := []string{"\nzeta: 1", "\nalpha:", "\n  b: 2", "\n  a: 1", "\n  new-key: value"}
	last := -1
	for _, s := range order {
		i := strings.Index(out, s)
		if i < 0 || i < last {
			t.Fatalf("expected %v in that order, got\n%s", order, out)
		}
		last = i
	}
	if !strings.Contains(out, "\n  b: 2") {
		t.Errorf("expected the original two space indentation, got\n%s", out)
	}
}

// Values inside flow collections are updated through the node tree.
func TestUpdateYaml_FlowCollection(t *testing.T) {
	src := "# limits\nlimits: {cpu: 1, memory: 512}\n"
	out, _ := updateYamlString(t, `[{"match": "limits.memory", "replace_with": "1024"}]`, src)
	v := decodeYamlString(t, out)
	limits, _ := v["limits"].(map[string]interface{})
	if limits["memory"] != 1024 || limits["cpu"] != 1 {
		t.Fatalf("unexpected limits %#v in\n%s", v["limits"], out)
	}
	if !strings.Contains(out, "# limits") {
		t.Errorf("expected the comment to be kept, got\n%s", out)
	}
}

func TestUpdateYaml_PreservesCRLF(t *testing.T) {
	src := "# comment\r\nserver: global\r\nport: 25565\r\n"
	out, _ := updateYamlString(t, `[{"match": "server", "replace_with": "lobby"}]`, src)
	if want := "# comment\r\nserver: lobby\r\nport: 25565\r\n"; out != want {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestUpdateYaml_EmptyDocument(t *testing.T) {
	out, changed := updateYamlString(t, `[{"match": "server-port", "replace_with": "25565"}]`, "")
	if !changed {
		t.Fatal("expected the document to change")
	}
	if v := decodeYamlString(t, out); v["server-port"] != 25565 {
		t.Fatalf("unexpected output\n%s", out)
	}
}

func TestUpdateYaml_Sequences(t *testing.T) {
	src := "# hosts\nhosts:\n  - name: a # first\n    ip: 127.0.0.1\n  - name: b\n    ip: 127.0.0.1\n"
	out, _ := updateYamlString(t, `[{"match": "hosts.*.ip", "replace_with": "0.0.0.0"}]`, src)
	if want := strings.ReplaceAll(src, "127.0.0.1", "0.0.0.0"); out != want {
		t.Fatalf("unexpected output\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// A new mapping holding a number is added through the node tree, so comments and
// key order are kept, the same as for a mapping holding a string.
func TestUpdateYaml_AddedMappingWithNumber(t *testing.T) {
	src := "# keep me\nb: x\nc: 1 # and me\n"
	out, _ := updateYamlString(t, `[{"match": "added.port", "replace_with": 25565}]`, src)
	if want := src + "added:\n  port: 25565\n"; out != want {
		t.Fatalf("unexpected output\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// A whole float such as 1.0 is patched in place and stays a float. Checking the
// patched document through JSON would read it back as the integer 1, reject the
// patch and fall back to writing the whole document without its comments.
func TestUpdateYaml_WholeFloat(t *testing.T) {
	src := "# rates\nrate: 2 # per tick\nscale: 1.0\nname: x\n"
	for _, replace := range []string{
		`[{"match": "rate", "replace_with": 1.0}]`,
		`[{"match": "rate", "replace_with": "1.0"}]`,
	} {
		out, changed := updateYamlString(t, replace, src)
		if !changed {
			t.Fatalf("%s: expected the document to change", replace)
		}
		if want := strings.Replace(src, "rate: 2", "rate: 1.0", 1); out != want {
			t.Fatalf("%s: unexpected output\n--- got ---\n%s\n--- want ---\n%s", replace, out, want)
		}
	}
}

// A timestamp decodes as a time.Time but reaches the replacements as a string.
// Verification must treat the two as equal, or changing any other key falls
// back to writing the whole document, quoting the date and dropping comments.
func TestUpdateYaml_Timestamps(t *testing.T) {
	src := "# dates\ncreated: 2024-01-01\nupdated: 2024-01-01T10:00:00.5+02:00\ntagged: !!timestamp 2024-01-01\nname: a # keep\n"
	out, changed := updateYamlString(t, `[{"match": "name", "replace_with": "b"}]`, src)
	if !changed {
		t.Fatal("expected the document to change")
	}
	if want := strings.Replace(src, "name: a", "name: b", 1); out != want {
		t.Fatalf("unexpected output\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}
