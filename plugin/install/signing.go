package install

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/pelican/wings/config"
)

// StageOption changes how StageAdd, StageUpdate and the import stagers treat
// an archive.
type StageOption func(*stageOptions)

type stageOptions struct {
	requireTrustedSignature bool
}

// RequireTrustedSignature refuses an archive that is not signed by a key in
// plugins.trusted_keys, whatever plugins.require_signatures says. The Panel
// API stages with it at the "signed" access level, so the Panel can only
// bring code the operator's keys vouch for.
func RequireTrustedSignature() StageOption {
	return func(o *stageOptions) { o.requireTrustedSignature = true }
}

func applyStageOptions(opts []StageOption) stageOptions {
	var o stageOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// enforceSignature applies the signing policy to a downloaded archive's digest:
//   - no signature + not required  -> allowed
//   - no signature + required      -> rejected
//   - a signature present          -> must verify against a trusted key,
//     regardless of the policy (a signature that does not verify means tampering)
//
// A signature is required when plugins.require_signatures is set or the caller
// staged with RequireTrustedSignature. verified reports that a signature was
// present and verified against a key in plugins.trusted_keys.
//
// The archive is signed over its sha256 digest, so a tampered archive (different
// digest) fails verification even if its sha256 matches a forged manifest.
func enforceSignature(digest []byte, signature string, o stageOptions) (verified bool, err error) {
	c := config.Get().Plugins
	if signature == "" {
		if o.requireTrustedSignature {
			return false, errors.New("plugin: the archive must be signed by a key in plugins.trusted_keys, and it is unsigned")
		}
		if c.RequireSignatures {
			return false, errors.New("plugin: a signature is required but the archive is unsigned")
		}
		return false, nil
	}
	if o.requireTrustedSignature && len(c.TrustedKeys) == 0 {
		return false, errors.New("plugin: the archive must be signed by a trusted key, and plugins.trusted_keys is empty")
	}
	if err := verifySignature(digest, signature, c.TrustedKeys); err != nil {
		return false, err
	}
	return true, nil
}

func verifySignature(digest []byte, signatureB64 string, trustedKeys []string) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signatureB64))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("plugin: malformed signature")
	}
	for _, k := range trustedKeys {
		pub, err := parsePublicKey(k)
		if err != nil {
			continue
		}
		if ed25519.Verify(pub, digest, sig) {
			return nil
		}
	}
	return errors.New("plugin: archive is not signed by any trusted key")
}

// parsePublicKey accepts "id:base64" or "base64" and returns the Ed25519 key.
func parsePublicKey(s string) (ed25519.PublicKey, error) {
	if _, key, ok := strings.Cut(s, ":"); ok {
		s = key
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("plugin: public key must be %d bytes", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// GenerateKeypair returns a new Ed25519 keypair as base64 strings (public,
// private) for the `wings plugins keygen` command.
func GenerateKeypair() (pub string, priv string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(pk), base64.StdEncoding.EncodeToString(sk), nil
}

// SignArchive signs the sha256 digest of r with a base64 Ed25519 private key and
// returns the base64 signature for a registry/update entry.
func SignArchive(privB64 string, r io.Reader) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privB64))
	if err != nil {
		return "", err
	}
	if len(raw) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("plugin: private key must be %d bytes", ed25519.PrivateKeySize)
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	sig := ed25519.Sign(ed25519.PrivateKey(raw), h.Sum(nil))
	return base64.StdEncoding.EncodeToString(sig), nil
}
