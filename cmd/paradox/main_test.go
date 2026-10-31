package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rumeasiyan/paradox"
	"github.com/Rumeasiyan/paradox/sign"
)

func noEnv(string) string { return "" }

func runArgs(t *testing.T, getenv func(string) string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut, getenv)
	return code, out.String(), errOut.String()
}

func TestVersionFlagPrintsVersionFile(t *testing.T) {
	code, out, errOut := runArgs(t, noEnv, "-version")
	if code != 0 || strings.TrimSpace(out) != paradox.Version {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
}

func TestNoArgumentsPrintsUsageAndFails(t *testing.T) {
	code, _, errOut := runArgs(t, noEnv)
	if code != 2 || !strings.Contains(errOut, "keygen -out FILE") {
		t.Fatalf("exit %d, usage %q", code, errOut)
	}
}

func TestUnknownFlagFails(t *testing.T) {
	if code, _, _ := runArgs(t, noEnv, "-nope"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestKeygenWritesPrivateFileAndPrintsPublic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "citadel.key")
	code, out, errOut := runArgs(t, noEnv, "keygen", "-out", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	pub, err := sign.ParsePublicKey(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("stdout is not a public key: %v", err)
	}
	if strings.Contains(out+errOut, "ed25519-priv:") {
		t.Fatal("private key reached the terminal")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("private key file mode %o, want 600", perm)
	}
	data, _ := os.ReadFile(path)
	priv, err := sign.ParsePrivateKey(string(data))
	if err != nil || !pub.Equal(priv.Public()) {
		t.Fatalf("file does not hold the matching private key: %v", err)
	}
}

func TestKeygenNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.key")
	os.WriteFile(path, []byte("keep me"), 0o600)
	code, _, errOut := runArgs(t, noEnv, "keygen", "-out", path)
	if code != 1 || !strings.Contains(errOut, "refusing to overwrite") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me" {
		t.Fatal("existing file was changed")
	}
}

func TestKeygenNeedsOut(t *testing.T) {
	if code, _, _ := runArgs(t, noEnv, "keygen"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestPubkeyFromEnvAndFile(t *testing.T) {
	pubText, privText, _ := sign.GenerateKey()

	env := func(k string) string {
		if k == "PARADOX_PRIVATE_KEY" {
			return privText
		}
		return ""
	}
	code, out, errOut := runArgs(t, env, "pubkey")
	if code != 0 || strings.TrimSpace(out) != pubText {
		t.Fatalf("from env: exit %d, out %q, err %q", code, out, errOut)
	}

	path := filepath.Join(t.TempDir(), "paradox.key")
	os.WriteFile(path, []byte(privText+"\n"), 0o600)
	code, out, _ = runArgs(t, noEnv, "pubkey", "-in", path)
	if code != 0 || strings.TrimSpace(out) != pubText {
		t.Fatalf("from file: exit %d, out %q", code, out)
	}
}

func TestPubkeyErrorsHideTheKey(t *testing.T) {
	code, _, errOut := runArgs(t, noEnv, "pubkey")
	if code != 1 || !strings.Contains(errOut, "PARADOX_PRIVATE_KEY is empty") {
		t.Fatalf("empty: exit %d: %s", code, errOut)
	}
	bad := func(string) string { return "ed25519-priv:c2VjcmV0" }
	code, _, errOut = runArgs(t, bad, "pubkey")
	if code != 1 || strings.Contains(errOut, "c2VjcmV0") {
		t.Fatalf("bad key: exit %d: %s", code, errOut)
	}
}
