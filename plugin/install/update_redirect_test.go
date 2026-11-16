package install

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pelican/wings/plugin"
)

// useTestTransport points the plugin HTTP client at the httptest certificate
// for the duration of a test. Every httptest TLS server shares that certificate.
func useTestTransport(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := updateHTTPClient.Transport
	updateHTTPClient.Transport = srv.Client().Transport
	t.Cleanup(func() { updateHTTPClient.Transport = prev })
}

func TestHTTPSGetRefusesRedirectToHTTP(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		_, _ = io.WriteString(w, "archive")
	}))
	defer plain.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/archive.tar.gz", http.StatusFound)
	}))
	defer secure.Close()
	useTestTransport(t, secure)

	body, err := httpsGet(context.Background(), secure.URL+"/archive.tar.gz", 1<<20, auth{token: "secret"})
	if err == nil {
		body.Close()
		t.Fatal("expected a redirect to http to be refused")
	}
	if !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("the http target was requested %d time(s)", n)
	}
}

func TestHTTPSGetDropsCredentialsWhenRedirectLeavesHost(t *testing.T) {
	type seen struct{ authz, key string }
	var other seen
	mirror := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other = seen{r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")}
		_, _ = io.WriteString(w, "archive")
	}))
	defer mirror.Close()

	var first, sameHost seen
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			first = seen{r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")}
			http.Redirect(w, r, "/moved", http.StatusFound)
		case "/moved":
			sameHost = seen{r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")}
			// Same IP, different port: a different host as far as the request is concerned.
			http.Redirect(w, r, mirror.URL+"/archive.tar.gz", http.StatusFound)
		}
	}))
	defer registry.Close()
	useTestTransport(t, registry)

	a := auth{token: "secret", headers: map[string]string{"X-Api-Key": "key"}}
	body, err := httpsGet(context.Background(), registry.URL+"/start", 1<<20, a)
	if err != nil {
		t.Fatalf("httpsGet: %v", err)
	}
	got, _ := io.ReadAll(body)
	body.Close()

	want := seen{"Bearer secret", "key"}
	if first != want || sameHost != want {
		t.Fatalf("credentials should reach the registry host: first=%+v sameHost=%+v", first, sameHost)
	}
	if other != (seen{}) {
		t.Fatalf("credentials leaked to another host: %+v", other)
	}
	if string(got) != "archive" {
		t.Fatalf("body = %q", got)
	}
}

// TestCheckUpdateReadsThePelicanHubFormat covers the update manifest the
// Pelican Hub already serves at /plugins/{slug}/update.json: the release sits
// under a "*" key and its URL is "download_url". With no release the Hub
// answers [].
func TestCheckUpdateReadsThePelicanHubFormat(t *testing.T) {
	var body string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	useTestTransport(t, srv)
	m := plugin.Manifest{Name: "demo", Version: "1.0.0", Update: srv.URL + "/plugins/demo/update.json"}

	body = `{"*":{"version":"1.1.0","download_url":"https://hub.example/plugins/demo/download/7/demo-1.1.0.zip","channel":"stable","sha256":"abc"}}`
	st, err := CheckUpdate(context.Background(), m)
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if !st.Available || st.Latest != "1.1.0" || st.download != "https://hub.example/plugins/demo/download/7/demo-1.1.0.zip" || st.sha256 != "abc" {
		t.Fatalf("status = %+v (download %q, sha256 %q)", st, st.download, st.sha256)
	}

	body = `[]`
	if _, err := CheckUpdate(context.Background(), m); err == nil || !strings.Contains(err.Error(), "no release") {
		t.Fatalf("an empty Hub manifest should say there is no release, got %v", err)
	}

	// The plain Wings format still works.
	body = `{"version":"1.2.0","download":"https://a.example/demo.tar.gz","sha256":"def"}`
	if st, err := CheckUpdate(context.Background(), m); err != nil || st.Latest != "1.2.0" || st.download != "https://a.example/demo.tar.gz" {
		t.Fatalf("plain manifest: %+v, %v", st, err)
	}
}
