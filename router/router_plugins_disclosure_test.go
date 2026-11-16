package router

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
	"github.com/pelican/wings/plugin/install"
)

// useProcessUser returns the user the test expects external plugins to run
// as: system.user set to nobody when the test runs as root, the test's own
// user otherwise.
func useProcessUser(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		c := config.Get()
		c.System.User.Uid, c.System.User.Gid = 65534, 65534
		config.Set(c)
		return "nobody"
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

type nativeDemo struct{}

func (nativeDemo) Name() string                                { return "native-demo" }
func (nativeDemo) Events() []string                            { return []string{"server.state"} }
func (nativeDemo) OnEvent(context.Context, plugin.Event) error { return nil }

// TestPluginAPIListShowsFormAndProcessUser covers meta.form,
// meta.binary_files and meta.runs_as (contract A3, S).
func TestPluginAPIListShowsFormAndProcessUser(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessView))
	runsAs := useProcessUser(t)
	if err := os.WriteFile(filepath.Join(a.dir, "beta", "blob"), []byte("\x7fELF\x02\x01\x01\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	plugin.RegisterEventListener(nativeDemo{})
	t.Cleanup(func() { plugin.Default().RemoveEventListener(nativeDemo{}) })

	rec := a.do(http.MethodGet, "/api/plugins", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				Form        *string   `json:"form"`
				BinaryFiles *[]string `json:"binary_files"`
				RunsAs      *string   `json:"runs_as"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		form   string
		binary []string
		runsAs string
	}{
		"alpha":       {"source", []string{}, runsAs},
		"beta":        {"binary", []string{"blob"}, runsAs},
		"native-demo": {"native", []string{}, plugin.WingsUser().Name},
	}
	seen := 0
	for _, p := range body.Data {
		w, ok := want[p.ID]
		if !ok {
			continue
		}
		seen++
		m := p.Meta
		if m.Form == nil || m.BinaryFiles == nil || m.RunsAs == nil {
			t.Fatalf("%s: meta.form, meta.binary_files or meta.runs_as missing (binary_files must be [] not null): %s", p.ID, rec.Body)
		}
		if *m.Form != w.form || strings.Join(*m.BinaryFiles, ",") != strings.Join(w.binary, ",") || *m.RunsAs != w.runsAs {
			t.Errorf("%s: form %q, binary_files %q, runs_as %q; want %q, %q, %q", p.ID, *m.Form, *m.BinaryFiles, *m.RunsAs, w.form, w.binary, w.runsAs)
		}
	}
	if seen != len(want) {
		t.Fatalf("listed %d of %d plugins: %s", seen, len(want), rec.Body)
	}
}

// TestPluginAPIStagedInstallShowsFormAndProcessUser covers form,
// binary_files and runs_as in the 202 response (contract A4), for an archive
// packaged in one top-level directory (R1).
func TestPluginAPIStagedInstallShowsFormAndProcessUser(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessSigned))
	runsAs := useProcessUser(t)
	pub, priv, _ := install.GenerateKeypair()
	a.registry(tarGz(t, map[string]string{
		"gamma-1.0.0/plugin.yaml": "apiVersion: v1\nname: gamma\ncapability: events\nversion: 1.0.0\nentrypoint: [\"./run\"]\n",
		"gamma-1.0.0/run":         "#!/bin/sh\n",
		"gamma-1.0.0/bin/helper":  "\x7fELF\x02\x01\x01\x00",
	}), pub, priv)

	rec, staged := a.json(http.MethodPost, "/api/plugins/gamma/install", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("stage: %d %s", rec.Code, rec.Body)
	}
	if staged["form"] != "binary" || staged["runs_as"] != runsAs {
		t.Fatalf("form %v, runs_as %v; want binary, %s: %s", staged["form"], staged["runs_as"], runsAs, rec.Body)
	}
	if bf, _ := staged["binary_files"].([]any); len(bf) != 1 || bf[0] != "bin/helper" {
		t.Fatalf("binary_files = %v, want [bin/helper]", staged["binary_files"])
	}
	if strings.Contains(rec.Body.String(), ".wings-install") {
		t.Fatalf("the response shows the staging path: %s", rec.Body)
	}

	token := staged["approval_token"].(string)
	rec, done := a.json(http.MethodPost, "/api/plugins/gamma/install", `{"approve":"`+token+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	meta := done["plugin"].(map[string]any)["meta"].(map[string]any)
	if meta["form"] != "binary" || meta["runs_as"] != runsAs {
		t.Fatalf("installed plugin meta = %v", meta)
	}
}

func TestPluginAPIStagedSourcePluginHasNoBinaryFiles(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessSigned))
	useProcessUser(t)
	pub, priv, _ := install.GenerateKeypair()
	a.registry(gammaArchive(t, "1.0.0"), pub, priv)
	rec, _ := a.json(http.MethodPost, "/api/plugins/gamma/install", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("stage: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"form":"source"`) || !strings.Contains(rec.Body.String(), `"binary_files":[]`) {
		t.Fatalf("want form source with binary_files []: %s", rec.Body)
	}
}

// TestPluginAPIRefusesAnArchiveLayoutWithoutTheStagingPath covers R1 through
// the Panel API: the 422 says what is wrong with the layout and shows no
// internal path.
func TestPluginAPIRefusesAnArchiveLayoutWithoutTheStagingPath(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessSigned))
	pub, priv, _ := install.GenerateKeypair()
	a.registry(tarGz(t, map[string]string{
		"gamma/plugin.yaml": "apiVersion: v1\nname: gamma\ncapability: events\nentrypoint: [\"./run\"]\n",
		"README.md":         "x",
	}), pub, priv)
	rec, _ := a.json(http.MethodPost, "/api/plugins/gamma/install", "")
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "layout") {
		t.Fatalf("want 422 about the layout: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), ".wings-install") || strings.Contains(rec.Body.String(), a.dir) {
		t.Fatalf("the error shows an internal path: %s", rec.Body)
	}
}

// TestPluginAPISourceCallsANULFileBinary keeps the source view consistent
// with meta.binary_files: contract S counts a NUL byte as not text, though
// it is valid UTF-8.
func TestPluginAPISourceCallsANULFileBinary(t *testing.T) {
	a := newPluginAPI(t, string(plugin.PanelAccessView))
	if err := os.WriteFile(filepath.Join(a.dir, "alpha", "data"), []byte("abc\x00def"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, body := a.json(http.MethodGet, "/api/plugins/alpha/source?file=data", "")
	if rec.Code != http.StatusOK || body["binary"] != true || body["content"] != nil {
		t.Fatalf("a file with a NUL byte: %d %s; want binary without content", rec.Code, rec.Body)
	}
}
