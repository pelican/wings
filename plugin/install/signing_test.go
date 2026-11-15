package install

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/pelican/wings/config"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("a plugin archive")
	sig, err := SignArchive(priv, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	digest := sha256.Sum256(data)

	if err := verifySignature(digest[:], sig, []string{"example:" + pub}); err != nil {
		t.Fatalf("trusted key should verify: %v", err)
	}

	// An untrusted key must not verify.
	other, _, _ := GenerateKeypair()
	if err := verifySignature(digest[:], sig, []string{other}); err == nil {
		t.Fatal("untrusted key must be rejected")
	}

	// A tampered archive (different digest) must not verify.
	bad := sha256.Sum256([]byte("tampered"))
	if err := verifySignature(bad[:], sig, []string{pub}); err == nil {
		t.Fatal("tampered digest must be rejected")
	}
}

func TestEnforceSignaturePolicy(t *testing.T) {
	c := setConfig(t) // require_signatures defaults to false, no trusted keys

	digest := sha256.Sum256([]byte("x"))

	// Unsigned + not required -> allowed.
	if err := enforceSignature(digest[:], "", stageOptions{}); err != nil {
		t.Fatalf("unsigned should be allowed when not required: %v", err)
	}

	// Unsigned + required -> rejected.
	c.Plugins.RequireSignatures = true
	config.Set(c)
	if err := enforceSignature(digest[:], "", stageOptions{}); err == nil {
		t.Fatal("unsigned must be rejected when signatures are required")
	}
}
