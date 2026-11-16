package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pelican/wings/config"
)

// pluginArchive builds a .tar.gz holding a plugin.yaml for name at version.
func pluginArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	manifest := "apiVersion: v1\nname: " + name + "\ncapability: events\nversion: " + version + "\nentrypoint: [\"./run\"]\n"
	for _, f := range []struct{ name, body string }{{"plugin.yaml", manifest}, {"run", "#!/bin/sh\n"}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveArchive serves body over https and returns a registry entry for it.
func serveArchive(t *testing.T, name string, body []byte, signature string) ResolvedPlugin {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(srv.Close)
	useTestTransport(t, srv)
	sum := sha256.Sum256(body)
	return ResolvedPlugin{Entry: CatalogEntry{
		Name: name, Version: "1.0.0", Download: srv.URL + "/" + name + ".tar.gz",
		SHA256: hex.EncodeToString(sum[:]), Signature: signature,
	}}
}

func TestRequireTrustedSignatureOverridesThePolicy(t *testing.T) {
	pub, priv, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, _ := GenerateKeypair()
	c := setConfig(t)
	c.Plugins.RequireSignatures = false
	c.Plugins.TrustedKeys = []string{"ops:" + pub}
	config.Set(c)

	body := pluginArchive(t, "demo", "1.0.0")
	sign := func(key string) string {
		sig, err := SignArchive(key, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	ctx := context.Background()

	// Without the option an unsigned archive is fine, as the policy says.
	stage, err := StageAdd(ctx, serveArchive(t, "demo", body, ""), t.TempDir())
	if err != nil {
		t.Fatalf("unsigned archive without the option: %v", err)
	}
	_ = stage.Cleanup()

	tests := []struct {
		name, signature, wantErr string
	}{
		{"unsigned", "", "unsigned"},
		{"signed by an unknown key", sign(otherPriv), "not signed by any trusted key"},
		{"signed by a trusted key", sign(priv), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stage, err := StageAdd(ctx, serveArchive(t, "demo", body, tt.signature), t.TempDir(), RequireTrustedSignature())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("StageAdd: %v", err)
				}
				_ = stage.Cleanup()
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("StageAdd error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestRequireTrustedSignatureWithNoTrustedKeys(t *testing.T) {
	_, priv, _ := GenerateKeypair()
	setConfig(t)
	body := pluginArchive(t, "demo", "1.0.0")
	sig, _ := SignArchive(priv, bytes.NewReader(body))
	_, err := StageAdd(context.Background(), serveArchive(t, "demo", body, sig), t.TempDir(), RequireTrustedSignature())
	if err == nil || !strings.Contains(err.Error(), "trusted_keys is empty") {
		t.Fatalf("StageAdd error = %v, want a refusal naming the empty trusted_keys", err)
	}
}
