package install

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelican/wings/config"
)

func writeArchive(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upload.tar.gz")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStageFileTakesTheNameFromTheManifest(t *testing.T) {
	setConfig(t)
	dir := t.TempDir()
	stage, err := StageFile(context.Background(), writeArchive(t, pluginArchive(t, "uploaded", "1.2.3")), "", dir)
	if err != nil {
		t.Fatalf("StageFile: %v", err)
	}
	defer stage.Cleanup()
	if stage.DestDir != filepath.Join(dir, "uploaded") {
		t.Fatalf("DestDir = %s, want the manifest's name under the plugins directory", stage.DestDir)
	}
	if filepath.Dir(stage.StagingDir) != dir || !strings.HasPrefix(filepath.Base(stage.StagingDir), ".wings-install-") {
		t.Fatalf("StagingDir = %s, want a hidden directory in the plugins directory", stage.StagingDir)
	}
	m, err := stage.Manifest()
	if err != nil || m.Version != "1.2.3" {
		t.Fatalf("Manifest = %+v, %v", m, err)
	}
}

func TestStageFileRefusesAManifestNamingTheParent(t *testing.T) {
	setConfig(t)
	dir := t.TempDir()
	_, err := StageFile(context.Background(), writeArchive(t, pluginArchive(t, "..", "1.0.0")), "", dir)
	if err == nil {
		t.Fatal("a manifest named .. was staged")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("staging left %d entries behind", len(entries))
	}
}

func TestStageFileAppliesTheSigningPolicy(t *testing.T) {
	c := setConfig(t)
	c.Plugins.RequireSignatures = true
	config.Set(c)
	_, err := StageFile(context.Background(), writeArchive(t, pluginArchive(t, "demo", "1.0.0")), "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("StageFile error = %v, want an unsigned archive refused under require_signatures", err)
	}
}

func TestStageURLChecksAGivenSHA256(t *testing.T) {
	setConfig(t)
	body := pluginArchive(t, "demo", "1.0.0")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	useTestTransport(t, srv)

	if _, err := StageURL(context.Background(), srv.URL+"/x.tar.gz", strings.Repeat("0", 64), "", t.TempDir()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("StageURL error = %v, want a checksum mismatch", err)
	}
	stage, err := StageURL(context.Background(), srv.URL+"/x.tar.gz", "", "", t.TempDir())
	if err != nil {
		t.Fatalf("StageURL without a sha256: %v", err)
	}
	_ = stage.Cleanup()
	if _, err := StageURL(context.Background(), "http://example.test/x.tar.gz", "", "", t.TempDir()); err == nil {
		t.Fatal("a plain http URL was fetched")
	}
}

func TestDigestChangesWithTheStagedContent(t *testing.T) {
	setConfig(t)
	stage, err := StageFile(context.Background(), writeArchive(t, pluginArchive(t, "demo", "1.0.0")), "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Cleanup()
	before, err := stage.Digest()
	if err != nil {
		t.Fatal(err)
	}
	again, _ := stage.Digest()
	if before != again {
		t.Fatal("Digest is not stable")
	}
	if err := os.WriteFile(filepath.Join(stage.StagingDir, "run"), []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if after, _ := stage.Digest(); after == before {
		t.Fatal("Digest did not change when a staged file did")
	}
}
