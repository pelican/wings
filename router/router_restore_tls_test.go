package router

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The restore client verifies a download against the TLS configuration the
// manager supplies (WithRestoreTLSConfig), and keeps its address checks.
func TestBackupRestoreHTTPClientUsesRestoreTLSConfig(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	setBackupRestoreAllowlist(t, []string{"127.0.0.1"})

	get := func(cfg *tls.Config) error {
		client := backupRestoreHttpClient(false, cfg)
		res, err := client.Get(srv.URL)
		if err == nil {
			res.Body.Close()
		}
		return err
	}
	if err := get(nil); err == nil {
		t.Fatal("a server signed by an unknown CA must not verify with the default configuration")
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	// The callback may return the same configuration for every restore, so the
	// client must not write to it (HTTP/2 setup adds to NextProtos).
	shared := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if err := get(shared); err != nil {
		t.Fatalf("the supplied configuration must be used: %v", err)
	}
	if len(shared.NextProtos) != 0 {
		t.Fatalf("the supplied configuration was modified: NextProtos = %v", shared.NextProtos)
	}

	setBackupRestoreAllowlist(t, nil)
	if err := get(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}); err == nil {
		t.Fatal("the address checks must still apply")
	}
}
