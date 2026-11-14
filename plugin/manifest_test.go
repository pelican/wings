package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverReadsManifestsWithoutExec(t *testing.T) {
	dir := t.TempDir()
	pdir := filepath.Join(dir, "audit")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "" +
		"apiVersion: v1\n" +
		"name: audit\n" +
		"capability: events\n" +
		"version: 1.0.0\n" +
		"entrypoint: [\"node\", \"index.js\"]\n" +
		"provides:\n" +
		"  events: [\"*\"]\n"
	if err := os.WriteFile(filepath.Join(pdir, ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory without a manifest is skipped rather than erroring.
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Discover(dir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(got))
	}
	m := got[0]
	if m.Name != "audit" || m.Capability != CapabilityEvents {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if len(m.Entrypoint) != 2 || m.Entrypoint[0] != "node" {
		t.Fatalf("unexpected entrypoint: %v", m.Entrypoint)
	}
	if m.Dir() != pdir {
		t.Fatalf("expected dir %s, got %s", pdir, m.Dir())
	}
}

func TestDiscoverReportsBadManifestAndContinues(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "good", ""+
		"apiVersion: v1\n"+
		"name: good\n"+
		"capability: events\n"+
		"entrypoint: [\"node\", \"index.js\"]\n")
	writeManifest(t, dir, "bad", "apiVersion: v1\n")

	got, issues, err := DiscoverDetailed(dir)
	if err != nil {
		t.Fatalf("DiscoverDetailed: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("expected good manifest to remain discoverable, got %+v", got)
	}
	if len(issues) != 1 {
		t.Fatalf("expected one discovery issue, got %d", len(issues))
	}

	if got, err := Discover(dir); len(got) != 1 || err == nil {
		t.Fatalf("Discover should return partial manifests plus aggregate error, got len=%d err=%v", len(got), err)
	}
}

func TestDiscoverRejectsDuplicateExternalNames(t *testing.T) {
	dir := t.TempDir()
	manifest := "" +
		"apiVersion: v1\n" +
		"name: dup\n" +
		"capability: events\n" +
		"entrypoint: [\"node\", \"index.js\"]\n"
	writeManifest(t, dir, "one", manifest)
	writeManifest(t, dir, "two", manifest)

	got, issues, err := DiscoverDetailed(dir)
	if err != nil {
		t.Fatalf("DiscoverDetailed: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("duplicate manifests should be omitted, got %+v", got)
	}
	if len(issues) != 2 {
		t.Fatalf("expected one issue per duplicate manifest, got %d", len(issues))
	}
}

func TestDiscoverMissingDirIsEmpty(t *testing.T) {
	got, err := Discover(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no manifests, got %d", len(got))
	}
}

func writeManifest(t *testing.T, root, name, manifest string) {
	t.Helper()
	pdir := filepath.Join(root, name)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadManifestRejectsIncomplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFile)
	// Missing name, capability and entrypoint.
	if err := os.WriteFile(path, []byte("apiVersion: v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(path); err == nil {
		t.Fatal("expected validation error for incomplete manifest")
	}
}

func TestDiscoverSkipsHiddenDirectories(t *testing.T) {
	dir := t.TempDir()
	manifest := "" +
		"apiVersion: v1\n" +
		"name: audit\n" +
		"capability: events\n" +
		"entrypoint: [\"node\", \"index.js\"]\n"
	writeManifest(t, dir, "audit", manifest)
	// What an interrupted install leaves behind: a second copy of the plugin.
	writeManifest(t, dir, ".wings-install-123", manifest)

	got, issues, err := DiscoverDetailed(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "audit" || len(issues) != 0 {
		t.Fatalf("a hidden leftover must not hide the real plugin: got %d manifests, issues %v", len(got), issues)
	}
}
