package parser

import (
	jsonv1 "encoding/json"
	jsonv2 "encoding/json/v2"
	"testing"

	goccy "github.com/goccy/go-json"
)

// configDecoders lists every way a ConfigurationFile can be decoded. goccy is
// the real path: remote/http.go decodes the Panel's server configuration with it.
var configDecoders = map[string]func([]byte, any) error{
	"goccy":   goccy.Unmarshal,
	"json/v1": jsonv1.Unmarshal,
	"json/v2": func(b []byte, v any) error { return jsonv2.Unmarshal(b, v) },
}

func decodeConfigFile(t *testing.T, name string, data string) (ConfigurationFile, error) {
	t.Helper()
	var f ConfigurationFile
	err := configDecoders[name]([]byte(data), &f)
	return f, err
}

func TestConfigurationFileUnmarshal(t *testing.T) {
	const egg = `{
		"file": "server.properties",
		"parser": "properties",
		"unknown": {"nested": [1, 2, {"x": null}]},
		"replace": [
			{"match": "server-ip", "replace_with": "0.0.0.0"},
			{"match": "server-port", "replace_with": 25565},
			{"match": "online-mode", "replace_with": true},
			{"match": "motd", "replace_with": "§aHello \"world\""},
			{"match": "legacy", "value": "old-egg"},
			{"match": "nothing", "replace_with": null},
			{"match": "obj", "replace_with": {"a": 1}},
			{"match": "cond", "if_value": "regex:^x$", "replace_with": "y"}
		]
	}`

	type want struct {
		match, ifValue, str string
		typ                 ValueType
	}
	wantReplace := []want{
		{"server-ip", "", "0.0.0.0", StringValue},
		{"server-port", "", "25565", NumberValue},
		{"online-mode", "", "true", BooleanValue},
		{"motd", "", "§aHello \"world\"", StringValue},
		{"legacy", "", "old-egg", StringValue},
		{"nothing", "", "<nil>", NullValue},
		{"obj", "", "<invalid>", ObjectValue},
		{"cond", "regex:^x$", "y", StringValue},
	}

	for name := range configDecoders {
		t.Run(name, func(t *testing.T) {
			f, err := decodeConfigFile(t, name, egg)
			if err != nil {
				t.Fatal(err)
			}
			if f.FileName != "server.properties" || f.Parser != Properties {
				t.Fatalf("file/parser = %q/%q", f.FileName, f.Parser)
			}
			if !f.AllowCreateFile {
				t.Error("create_file missing should default to true")
			}
			if len(f.Replace) != len(wantReplace) {
				t.Fatalf("got %d replacements, want %d", len(f.Replace), len(wantReplace))
			}
			for i, w := range wantReplace {
				r := f.Replace[i]
				if r.Match != w.match || r.IfValue != w.ifValue || r.ReplaceWith.Type() != w.typ || r.ReplaceWith.String() != w.str {
					t.Errorf("replace[%d] = {%q %q %v %q}, want {%q %q %v %q}", i,
						r.Match, r.IfValue, r.ReplaceWith.Type(), r.ReplaceWith.String(),
						w.match, w.ifValue, w.typ, w.str)
				}
				if string(r.ReplaceWith.Bytes()) != w.str {
					t.Errorf("replace[%d].Bytes() = %q, want %q", i, r.ReplaceWith.Bytes(), w.str)
				}
			}
		})
	}
}

func TestConfigurationFileUnmarshalEdgeCases(t *testing.T) {
	for name := range configDecoders {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeConfigFile(t, name, `{"parser":"yaml"}`); err == nil {
				t.Error("missing 'file' should fail")
			}
			if _, err := decodeConfigFile(t, name, `{"file":null,"parser":"yaml"}`); err == nil {
				t.Error("null 'file' should fail")
			}
			if _, err := decodeConfigFile(t, name, `{"file":"a.yml"}`); err == nil {
				t.Error("missing 'parser' should fail")
			}

			f, err := decodeConfigFile(t, name, `{"file":"a.yml","parser":"yaml","create_file":false}`)
			if err != nil {
				t.Fatal(err)
			}
			if f.AllowCreateFile {
				t.Error("create_file:false should be kept")
			}
			if f.Replace == nil || len(f.Replace) != 0 {
				t.Errorf("missing replace should give an empty, non-nil slice, got %#v", f.Replace)
			}

			f, err = decodeConfigFile(t, name, `{"file":"a.yml","parser":"yaml","create_file":"nope"}`)
			if err != nil {
				t.Fatal(err)
			}
			if !f.AllowCreateFile {
				t.Error("invalid create_file should fall back to true")
			}

			// A broken replacement is logged and dropped, not fatal.
			f, err = decodeConfigFile(t, name, `{"file":"a.yml","parser":"yaml","replace":[{"replace_with":"x"}]}`)
			if err != nil {
				t.Fatalf("bad replacement should not fail the file: %v", err)
			}
			if f.Replace == nil || len(f.Replace) != 0 {
				t.Errorf("bad replacement should give an empty slice, got %#v", f.Replace)
			}

			// replace_with wins over the legacy value key regardless of order.
			f, err = decodeConfigFile(t, name, `{"file":"a.yml","parser":"yaml","replace":[{"value":"old","match":"k","replace_with":"new"}]}`)
			if err != nil {
				t.Fatal(err)
			}
			if got := f.Replace[0].ReplaceWith.String(); got != "new" {
				t.Errorf("replace_with should win over value, got %q", got)
			}
		})
	}
}

// Egg JSON from the Panel must keep encoding/json v1 semantics: the last of
// any duplicate keys wins, and invalid UTF-8 does not fail the decode.
func TestConfigurationFileUnmarshalLegacySemantics(t *testing.T) {
	for _, name := range []string{"goccy", "json/v1"} {
		t.Run(name, func(t *testing.T) {
			f, err := decodeConfigFile(t, name, `{"file":"a.yml","file":"b.yml","parser":"yaml","replace":[{"match":"k","replace_with":"1","replace_with":"2"}]}`)
			if err != nil {
				t.Fatal(err)
			}
			if f.FileName != "b.yml" || f.Replace[0].ReplaceWith.String() != "2" {
				t.Errorf("last duplicate key should win, got file=%q value=%q", f.FileName, f.Replace[0].ReplaceWith.String())
			}

			f, err = decodeConfigFile(t, name, "{\"file\":\"caf\xe9.yml\",\"parser\":\"yaml\"}")
			if err != nil {
				t.Fatalf("invalid UTF-8 should not fail: %v", err)
			}
			if f.FileName == "" {
				t.Error("file name with invalid UTF-8 was dropped")
			}
		})
	}
}

func TestLookupConfigurationValue(t *testing.T) {
	f := ConfigurationFile{configuration: []byte(`{"docker":{"interface":"172.18.0.1","network":{"interface":"172.18.0.1","mtu":1500,"name":"pel\"ican"}}}`)}

	cases := []struct{ in, want string }{
		{`"{{config.docker.interface}}"`, "172.18.0.1"},
		{`"ip={{ config.docker.network.interface }}:25565"`, "ip=172.18.0.1:25565"},
		{`"{{config.docker.network.mtu}}"`, "1500"},
		{`"{{config.docker.network.name}}"`, `pel"ican`},
		// Objects, arrays and missing keys leave the placeholder intact.
		{`"{{config.docker.network}}"`, "{{config.docker.network}}"},
		{`"{{config.docker.missing}}"`, "{{config.docker.missing}}"},
		// Non-string values are returned as-is.
		{`8080`, "8080"},
	}
	for _, c := range cases {
		var r ConfigurationFileReplacement
		if err := goccy.Unmarshal([]byte(`{"match":"k","replace_with":`+c.in+`}`), &r); err != nil {
			t.Fatal(err)
		}
		got, err := f.LookupConfigurationValue(r)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.in, got, c.want)
		}
	}
}
