package diagnostics

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/pelican/wings/config"
)

func redactTestConfig() *config.Configuration {
	c := &config.Configuration{
		AuthenticationToken: "node-secret-token-value",
		PanelLocation:       "https://panel.example.com",
	}
	c.Api.Host = "203.0.113.10"
	c.Api.Ssl.CertificateFile = "/etc/letsencrypt/live/node.example.com/fullchain.pem"
	c.System.Sftp.Address = "0.0.0.0"
	return c
}

// Endpoints are removed from the whole report, including the logs, unless they
// should be included.
func TestRedactReportRemovesEndpointsEverywhere(t *testing.T) {
	report := strings.Join([]string{
		"Panel Location: https://panel.example.com",
		"log: failed to reach panel.example.com from 203.0.113.10",
		"cert: /etc/letsencrypt/live/node.example.com/fullchain.pem",
		"listening on 0.0.0.0:2022",
	}, "\n")

	got := redactReport(report, redactTestConfig(), false)
	for _, s := range []string{"panel.example.com", "203.0.113.10", "fullchain.pem"} {
		if strings.Contains(got, s) {
			t.Errorf("expected %q to be redacted, got:\n%s", s, got)
		}
	}
	if !strings.Contains(got, "0.0.0.0:2022") {
		t.Errorf("expected the wildcard address to be kept, got:\n%s", got)
	}

	if kept := redactReport(report, redactTestConfig(), true); !strings.Contains(kept, "panel.example.com") {
		t.Errorf("expected endpoints to be kept when they are included, got:\n%s", kept)
	}
}

// Tokens passed in request URLs, and JWTs anywhere else, are never included.
func TestRedactReportRemovesQueryTokens(t *testing.T) {
	// Built from its parts so that it is not mistaken for a leaked credential.
	enc := base64.RawURLEncoding.EncodeToString
	jwt := enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc([]byte(`{"server_uuid":"x"}`)) + "." + enc([]byte("signature"))
	report := strings.Join([]string{
		`GET /api/servers/x/ws?token=` + jwt + `&foo=1 200`,
		`GET /download/backup?a=b&token=opaque-value-123 200`,
		// The key is percent-encoded, which the download handler decodes.
		`GET /download/file?%74oken=%65yJencoded.value.sig&x=2 200`,
		`GET /download/file?TOKEN=upper-value 200`,
		`bearer ` + jwt,
	}, "\n")
	for _, include := range []bool{true, false} {
		got := redactReport(report, redactTestConfig(), include)
		for _, s := range []string{jwt, "opaque-value-123", "eyJ", "%65yJ", "upper-value"} {
			if strings.Contains(got, s) {
				t.Errorf("includeEndpoints=%v: expected %q to be redacted, got:\n%s", include, s, got)
			}
		}
		for _, s := range []string{"&foo=1 200", "?a=b&token={redacted} 200", "?%74oken={redacted}&x=2 200"} {
			if !strings.Contains(got, s) {
				t.Errorf("includeEndpoints=%v: expected %q to be kept, got:\n%s", include, s, got)
			}
		}
	}
}

// The node token is never included.
func TestRedactReportAlwaysRemovesToken(t *testing.T) {
	report := "debug: Authorization: Bearer node-secret-token-value"
	for _, include := range []bool{true, false} {
		if got := redactReport(report, redactTestConfig(), include); strings.Contains(got, "node-secret-token-value") {
			t.Errorf("includeEndpoints=%v: expected the token to be redacted, got %q", include, got)
		}
	}
}
