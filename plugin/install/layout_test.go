package install

import (
	"archive/tar"
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const demoManifest = "apiVersion: v1\nname: demo\ncapability: events\nversion: 1.1.0\nentrypoint: [\"./bin/run\"]\n"

// archiveServer serves archive over https and returns its URL and sha256.
func archiveServer(t *testing.T, archive string) (url, sum string) {
	t.Helper()
	body, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	useTestTransport(t, srv)
	h := sha256.Sum256(body)
	return srv.URL + "/demo.archive", hex.EncodeToString(h[:])
}

type stageResult struct {
	pluginsDir string
	stage      *StagedInstall
	err        error
}

// stagePaths stages archive the ways a registry entry and an update manifest
// do, each into a fresh plugins directory, and returns the results by path.
// The URL and upload imports (StageURL, StageFile) go through the same
// staging step; they are covered where they are defined.
func stagePaths(t *testing.T, archive string) map[string]stageResult {
	t.Helper()
	setConfig(t)
	url, sum := archiveServer(t, archive)
	out := map[string]stageResult{}

	dir := t.TempDir()
	s, err := StageAdd(context.Background(), ResolvedPlugin{Entry: CatalogEntry{Name: "demo", Download: url, SHA256: sum}}, dir)
	out["add"] = stageResult{dir, s, err}

	dir = t.TempDir()
	pluginDir(t, filepath.Join(dir, "demo"), "demo v1")
	s, err = StageUpdate(context.Background(), UpdateStatus{Name: "demo", download: url, sha256: sum, dir: filepath.Join(dir, "demo")})
	out["update"] = stageResult{dir, s, err}
	return out
}

// nestedArchives are the layouts staging accepts: the manifest at the root,
// or inside one top-level directory with nothing else next to it (how GitHub
// release archives and the Hub package a plugin).
func nestedArchives(t *testing.T) map[string]string {
	return map[string]string{
		"tar.gz": writeTar(t, []tarEntry{
			{name: "demo-1.1.0/", typ: tar.TypeDir},
			{name: "demo-1.1.0/plugin.yaml", typ: tar.TypeReg, body: demoManifest},
			{name: "demo-1.1.0/bin/run", typ: tar.TypeReg, body: "#!/bin/sh\n"},
			{name: "demo-1.1.0/lib/x", typ: tar.TypeSymlink, link: "../bin/run"},
			// An inner directory named like the top-level one.
			{name: "demo-1.1.0/demo-1.1.0/notes.txt", typ: tar.TypeReg, body: "hi\n"},
		}),
		"zip": writeZip(t, map[string]string{
			"demo-main/plugin.yaml": demoManifest,
			"demo-main/bin/run":     "#!/bin/sh\n",
		}),
		"root": writeTar(t, []tarEntry{
			{name: "plugin.yaml", typ: tar.TypeReg, body: demoManifest},
			{name: "bin/run", typ: tar.TypeReg, body: "#!/bin/sh\n"},
		}),
	}
}

// checkNestedStage checks a stage of one of nestedArchives.
func checkNestedStage(t *testing.T, format string, stage *StagedInstall) {
	t.Helper()
	m, err := stage.Manifest()
	if err != nil || m.Name != "demo" {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	if _, err := os.Stat(filepath.Join(stage.StagingDir, "bin", "run")); err != nil {
		t.Fatalf("the plugin's files should be at the top of the staged directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stage.StagingDir, "demo-main")); err == nil {
		t.Fatal("the archive's top-level directory was left in place")
	}
	if format == "tar.gz" {
		if b, err := os.ReadFile(filepath.Join(stage.StagingDir, "lib", "x")); err != nil || string(b) != "#!/bin/sh\n" {
			t.Fatalf("a relative link inside the plugin should still resolve: %q, %v", b, err)
		}
		if _, err := os.Stat(filepath.Join(stage.StagingDir, "demo-1.1.0", "notes.txt")); err != nil {
			t.Fatalf("an inner directory named like the top-level one was lost: %v", err)
		}
	}
}

func TestStagingAcceptsAManifestInsideOneTopLevelDirectory(t *testing.T) {
	for format, archive := range nestedArchives(t) {
		for path, r := range stagePaths(t, archive) {
			t.Run(format+"/"+path, func(t *testing.T) {
				if r.err != nil {
					t.Fatalf("stage: %v", r.err)
				}
				defer r.stage.Cleanup()
				checkNestedStage(t, format, r.stage)
			})
		}
	}
}

// refusedLayouts are archives whose layout staging refuses.
func refusedLayouts(t *testing.T) map[string]string {
	return map[string]string{
		"two top-level directories": writeTar(t, []tarEntry{
			{name: "demo/plugin.yaml", typ: tar.TypeReg, body: demoManifest},
			{name: "other/readme", typ: tar.TypeReg, body: "x"},
		}),
		"a file next to the directory": writeTar(t, []tarEntry{
			{name: "demo/plugin.yaml", typ: tar.TypeReg, body: demoManifest},
			{name: "README.md", typ: tar.TypeReg, body: "x"},
		}),
		"manifest two levels down": writeTar(t, []tarEntry{
			{name: "a/b/plugin.yaml", typ: tar.TypeReg, body: demoManifest},
		}),
		"no manifest": writeTar(t, []tarEntry{
			{name: "demo/bin/run", typ: tar.TypeReg, body: "x"},
		}),
		"empty": writeTar(t, []tarEntry{{name: "./", typ: tar.TypeDir}}),
		"only a symlink to a directory": writeTar(t, []tarEntry{
			{name: "link", typ: tar.TypeSymlink, link: "."},
		}),
	}
}

func TestStagingRefusesOtherLayouts(t *testing.T) {
	for name, archive := range refusedLayouts(t) {
		for path, r := range stagePaths(t, archive) {
			t.Run(name+"/"+path, func(t *testing.T) {
				if r.err == nil {
					r.stage.Cleanup()
					t.Fatal("expected the layout to be refused")
				}
				t.Log(r.err)
				if !strings.Contains(r.err.Error(), "layout") {
					t.Fatalf("the error should be about the archive's layout: %v", r.err)
				}
				if strings.Contains(r.err.Error(), ".wings-install") {
					t.Fatalf("the error shows the staging path: %v", r.err)
				}
				if left := hiddenEntries(t, r.pluginsDir); len(left) != 0 {
					t.Fatalf("staging left behind: %v", left)
				}
			})
		}
	}
}

// TestStagingRefusesALinkThatLeavesAfterMovingUp covers a link that resolves
// inside the archive's top-level directory only while that directory is
// there: "demo/up -> .." is the staging root, but once demo's contents move
// up it would be the plugins directory.
func TestStagingRefusesALinkThatLeavesAfterMovingUp(t *testing.T) {
	archive := writeTar(t, []tarEntry{
		{name: "demo/plugin.yaml", typ: tar.TypeReg, body: demoManifest},
		{name: "demo/up", typ: tar.TypeSymlink, link: ".."},
	})
	for path, r := range stagePaths(t, archive) {
		if r.err == nil {
			r.stage.Cleanup()
			t.Fatalf("%s: a link leaving the plugin after moving up must be refused", path)
		}
		if strings.Contains(r.err.Error(), ".wings-install") {
			t.Fatalf("%s: the error shows the staging path: %v", path, r.err)
		}
		if left := hiddenEntries(t, r.pluginsDir); len(left) != 0 {
			t.Fatalf("%s: staging left behind: %v", path, left)
		}
	}
}

// badManifests are archives that stage but whose manifest can't be used.
func badManifests(t *testing.T) map[string]string {
	return map[string]string{
		"bad yaml":           writeTar(t, []tarEntry{{name: "plugin.yaml", typ: tar.TypeReg, body: "apiVersion: v1\nname: [demo\n"}}),
		"missing apiVersion": writeTar(t, []tarEntry{{name: "plugin.yaml", typ: tar.TypeReg, body: "name: demo\ncapability: events\nentrypoint: [x]\n"}}),
		"missing name":       writeTar(t, []tarEntry{{name: "plugin.yaml", typ: tar.TypeReg, body: "apiVersion: v1\ncapability: events\nentrypoint: [x]\n"}}),
		"invalid name":       writeTar(t, []tarEntry{{name: "plugin.yaml", typ: tar.TypeReg, body: "apiVersion: v1\nname: .demo\ncapability: events\nentrypoint: [x]\n"}}),
		"manifest is a directory": writeTar(t, []tarEntry{
			{name: "plugin.yaml/", typ: tar.TypeDir}, {name: "plugin.yaml/x", typ: tar.TypeReg, body: "x"},
		}),
		"nested, bad yaml": writeTar(t, []tarEntry{{name: "demo/plugin.yaml", typ: tar.TypeReg, body: "apiVersion: v1\nname: [demo\n"}}),
	}
}

// errorFrom returns the error staging or reading the staged manifest gave.
func errorFrom(r stageResult) error {
	if r.err != nil {
		return r.err
	}
	defer r.stage.Cleanup()
	_, err := r.stage.Manifest()
	return err
}

func TestStagingErrorsDoNotShowTheStagingPath(t *testing.T) {
	for name, archive := range badManifests(t) {
		for path, r := range stagePaths(t, archive) {
			t.Run(name+"/"+path, func(t *testing.T) {
				err := errorFrom(r)
				if err == nil {
					t.Fatal("expected an error")
				}
				t.Log(err)
				if strings.Contains(err.Error(), ".wings-install") || strings.Contains(err.Error(), r.pluginsDir) {
					t.Fatalf("the error shows an internal path: %v", err)
				}
			})
		}
	}
}

// TestStagingErrorFromCreatingTheStagingDirectory covers the first place a
// staging name appears: creating the directory itself.
func TestStagingErrorFromCreatingTheStagingDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write into a read-only directory")
	}
	archive := writeTar(t, []tarEntry{{name: "plugin.yaml", typ: tar.TypeReg, body: demoManifest}})
	setConfig(t)
	url, sum := archiveServer(t, archive)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, err := StageAdd(context.Background(), ResolvedPlugin{Entry: CatalogEntry{Name: "demo", Download: url, SHA256: sum}}, dir)
	if err == nil {
		t.Fatal("expected staging into a read-only directory to fail")
	}
	if strings.Contains(err.Error(), ".wings-install") {
		t.Fatalf("the error shows the staging directory's name: %v", err)
	}
}

func writeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
