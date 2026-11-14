package plugin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
