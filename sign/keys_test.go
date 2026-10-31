package sign

import (
	"strings"
	"testing"
)

func TestKeyTextRoundTrip(t *testing.T) {
	pubText, privText, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pubText, "ed25519-pub:") || !strings.HasPrefix(privText, "ed25519-priv:") {
		t.Fatalf("prefixes: %q %q", pubText[:12], privText[:13])
	}
	pub, err := ParsePublicKey(pubText)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePrivateKey(privText)
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Equal(priv.Public()) {
		t.Fatal("public key does not match private key")
	}
	if !strings.HasPrefix(Fingerprint(pub), "SHA256:") {
		t.Fatalf("fingerprint %q", Fingerprint(pub))
	}
}

func TestPrivateKeyInPublicFileIsRefused(t *testing.T) {
	_, privText, _ := GenerateKey()
	_, err := ParsePublicKey(privText)
	if err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), strings.TrimPrefix(privText, "ed25519-priv:")) {
		t.Fatal("error repeats the private key")
	}
}

func TestBadPrivateKeyErrorHidesValue(t *testing.T) {
	_, err := ParsePrivateKey("ed25519-priv:c2VjcmV0LXZhbHVl")
	if err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Fatalf("got %v", err)
	}
}

func TestSystemPubFile(t *testing.T) {
	a, _, _ := GenerateKey()
	b, _, _ := GenerateKey()
	c, _, _ := GenerateKey()

	keys, err := ParsePublicKeys("# current\n" + a + "\n\n# next\n" + b + "\n")
	if err != nil || len(keys) != 2 {
		t.Fatalf("two keys: %d, %v", len(keys), err)
	}
	if _, err := ParsePublicKeys("# nothing here\n"); err == nil {
		t.Fatal("empty file accepted")
	}
	if _, err := ParsePublicKeys(a + "\n" + b + "\n" + c + "\n"); err == nil || !strings.Contains(err.Error(), "at most two") {
		t.Fatalf("three keys: %v", err)
	}
	if _, err := ParsePublicKeys(a + "\nnot-a-key\n"); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("bad line: %v", err)
	}
}
