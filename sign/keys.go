package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Keys are written as one line of text, so they fit in an env file and in
// system.pub. The prefix makes a private key in a public-key file (or the
// reverse) a loud error instead of a silent mistake.
const (
	publicPrefix  = "ed25519-pub:"
	privatePrefix = "ed25519-priv:"
)

// GenerateKey makes a new key pair and returns both halves as text
// (public first). The private half must never leave its owner.
func GenerateKey() (public, private string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return FormatPublicKey(pub), privatePrefix + base64.StdEncoding.EncodeToString(priv.Seed()), nil
}

// FormatPublicKey writes a public key in the one-line text form.
func FormatPublicKey(k ed25519.PublicKey) string {
	return publicPrefix + base64.StdEncoding.EncodeToString(k)
}

// ParsePublicKey reads one public key in the one-line text form.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, privatePrefix) {
		return nil, errors.New("this is a private key; only public keys may be shared")
	}
	raw, ok := strings.CutPrefix(s, publicPrefix)
	if !ok {
		return nil, fmt.Errorf("public key must start with %q", publicPrefix)
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("public key is not 32 bytes of base64")
	}
	return ed25519.PublicKey(b), nil
}

// ParsePublicKeys reads a system.pub file: one or two keys, one per line,
// so a key can be changed without downtime (PX-SIG-06). Blank lines and
// lines starting with # are ignored.
func ParsePublicKeys(text string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := ParsePublicKey(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		keys = append(keys, k)
	}
	switch {
	case len(keys) == 0:
		return nil, errors.New("no public key found")
	case len(keys) > 2:
		return nil, fmt.Errorf("%d public keys found; at most two are allowed, the current one and the next", len(keys))
	}
	return keys, nil
}

// ParsePrivateKey reads a private key in the one-line text form. Error
// messages never include the key.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	s = strings.TrimSpace(s)
	raw, ok := strings.CutPrefix(s, privatePrefix)
	if !ok {
		return nil, fmt.Errorf("private key must start with %q", privatePrefix)
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, errors.New("private key is not 32 bytes of base64")
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// Fingerprint names a public key without revealing it, for logs and records.
func Fingerprint(k ed25519.PublicKey) string {
	sum := sha256.Sum256(k)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
