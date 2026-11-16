package install

import (
	"testing"

	"github.com/pelican/wings/config"
)

// setConfig installs a fresh test configuration as the global one.
func setConfig(t *testing.T) *config.Configuration {
	t.Helper()
	c, err := config.NewAtPath("test.yml")
	if err != nil {
		t.Fatalf("config.NewAtPath: %v", err)
	}
	// config.Set derives a JWT signing key from the token and panics on an empty
	// one, so give the test configuration a non-empty token.
	c.AuthenticationToken = "test-token"
	config.Set(c)
	return c
}
