package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRemoteToken(t *testing.T) {
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")

	cfg := Configuration{
		AuthenticationTokenId: "panel-id",
		AuthenticationToken:   "panel-token",
	}
	if err := cfg.ResolveToken(true); err != nil {
		t.Fatalf("expected remote credentials to resolve: %v", err)
	}
	if cfg.Token.ID != "panel-id" || cfg.Token.Token != "panel-token" {
		t.Fatalf("unexpected resolved credentials: %#v", cfg.Token)
	}
}

func TestResolveRemoteTokenRejectsIndirection(t *testing.T) {
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")

	tests := []Configuration{
		{AuthenticationTokenId: "file:///tmp/id", AuthenticationToken: "panel-token"},
		{AuthenticationTokenId: "panel-id", AuthenticationToken: "$PANEL_TOKEN"},
	}
	for _, cfg := range tests {
		if err := cfg.ResolveToken(true); err == nil {
			t.Fatal("expected remote token indirection to be rejected")
		}
	}
}

func TestResolveRemoteTokenRequiresEnvironmentMatch(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte("panel-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WINGS_TOKEN_ID", "panel-id")
	t.Setenv("WINGS_TOKEN", "file://"+secret)

	cfg := Configuration{
		AuthenticationTokenId: "panel-id",
		AuthenticationToken:   "panel-token",
	}
	if err := cfg.ResolveToken(true); err != nil {
		t.Fatalf("expected matching environment credentials to resolve: %v", err)
	}

	cfg.AuthenticationToken = "rotated-token"
	if err := cfg.ResolveToken(true); err == nil {
		t.Fatal("expected mismatched environment credentials to be rejected")
	}
}