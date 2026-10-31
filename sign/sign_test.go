package sign

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 1, 30, 0, 0, time.UTC)

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pubText, privText, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(pubText)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePrivateKey(privText)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func signed(t *testing.T, method, target string, body []byte, s Signer) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	if err := s.Sign(r, body); err != nil {
		t.Fatal(err)
	}
	return r
}

func verifierFor(id string, d Direction, keys ...ed25519.PublicKey) Verifier {
	return Verifier{
		Direction: d,
		Now:       fixed(t0),
		Keys: func(k string) []ed25519.PublicKey {
			if k == id {
				return keys
			}
			return nil
		},
	}
}

func TestRoundTripBothDirections(t *testing.T) {
	pub, priv := newKey(t)
	body := []byte(`{"job":"generate-certificates"}`)

	r := signed(t, "POST", "http://paradox:8080/api/jobs", body, Signer{KeyID: "citadel", Key: priv, Direction: ToParadox, Now: fixed(t0)})
	res, err := verifierFor("citadel", ToParadox, pub).Verify(r, body)
	if err != nil || res.KeyID != "citadel" || res.Fingerprint != Fingerprint(pub) {
		t.Fatalf("to PARADOX: %+v, %v", res, err)
	}

	r = signed(t, "POST", "http://citadel:8080/jobs/send-reminders", nil, Signer{KeyID: ParadoxKeyID, Key: priv, Direction: ToSystem, Now: fixed(t0)})
	if _, err := verifierFor(ParadoxKeyID, ToSystem, pub).Verify(r, nil); err != nil {
		t.Fatalf("to system: %v", err)
	}
}

func TestOneCallHelpers(t *testing.T) {
	pub, priv := newKey(t)
	// A system checks a call from PARADOX with one call (PX-CON-03).
	r := httptest.NewRequest("POST", "/jobs/send-reminders", nil)
	if err := (Signer{KeyID: ParadoxKeyID, Key: priv, Direction: ToSystem}).Sign(r, nil); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFromParadox(r, nil, pub); err != nil {
		t.Fatalf("VerifyFromParadox: %v", err)
	}
	// And signs its own queue request with one call.
	body := []byte(`{}`)
	r = httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
	if err := SignForParadox(r, body, "citadel", priv); err != nil {
		t.Fatal(err)
	}
	if _, err := (Verifier{Direction: ToParadox, Keys: func(string) []ed25519.PublicKey { return []ed25519.PublicKey{pub} }}).Verify(r, body); err != nil {
		t.Fatalf("SignForParadox: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, otherPriv := newKey(t)
	body := []byte(`{"data":1}`)
	good := Signer{KeyID: "citadel", Key: priv, Direction: ToParadox, Now: fixed(t0)}
	v := verifierFor("citadel", ToParadox, pub)

	cases := []struct {
		name string
		req  func() (*http.Request, []byte)
		v    Verifier
		want error
	}{
		{"body changed", func() (*http.Request, []byte) {
			return signed(t, "POST", "/api/jobs", body, good), []byte(`{"data":2}`)
		}, v, ErrDigest},
		{"path changed", func() (*http.Request, []byte) {
			r := signed(t, "POST", "/api/jobs", body, good)
			r.URL.Path = "/api/jobs/other"
			return r, body
		}, v, ErrBadSignature},
		{"method changed", func() (*http.Request, []byte) {
			r := signed(t, "POST", "/api/jobs", body, good)
			r.Method = "PUT"
			return r, body
		}, v, ErrBadSignature},
		{"key ID swapped", func() (*http.Request, []byte) {
			r := signed(t, "POST", "/api/jobs", body, good)
			r.Header.Set("Signature-Input", strings.Replace(r.Header.Get("Signature-Input"), `"citadel"`, `"other"`, 1))
			return r, body
		}, verifierFor("other", ToParadox, otherPub), ErrBadSignature},
		{"other system's key", func() (*http.Request, []byte) {
			return signed(t, "POST", "/api/jobs", body, Signer{KeyID: "citadel", Key: otherPriv, Direction: ToParadox, Now: fixed(t0)}), body
		}, v, ErrBadSignature},
		{"unknown system", func() (*http.Request, []byte) {
			return signed(t, "POST", "/api/jobs", body, Signer{KeyID: "stranger", Key: priv, Direction: ToParadox, Now: fixed(t0)}), body
		}, v, ErrUnknownKey},
		{"wrong direction (PX-SIG-04)", func() (*http.Request, []byte) {
			return signed(t, "POST", "/api/jobs", body, Signer{KeyID: "citadel", Key: priv, Direction: ToSystem, Now: fixed(t0)}), body
		}, v, ErrWrongDirection},
		{"too old (PX-SIG-03)", func() (*http.Request, []byte) {
			return signed(t, "POST", "/api/jobs", body, Signer{KeyID: "citadel", Key: priv, Direction: ToParadox, Now: fixed(t0.Add(-5*time.Minute - time.Second))}), body
		}, v, ErrExpired},
		{"too far ahead (PX-SIG-03)", func() (*http.Request, []byte) {
			return signed(t, "POST", "/api/jobs", body, Signer{KeyID: "citadel", Key: priv, Direction: ToParadox, Now: fixed(t0.Add(5*time.Minute + time.Second))}), body
		}, v, ErrExpired},
		{"no headers", func() (*http.Request, []byte) {
			return httptest.NewRequest("POST", "/api/jobs", nil), nil
		}, v, ErrMissing},
		{"extra component", func() (*http.Request, []byte) {
			r := signed(t, "POST", "/api/jobs", body, good)
			r.Header.Set("Signature-Input", strings.Replace(r.Header.Get("Signature-Input"), `"content-digest")`, `"content-digest" "@authority")`, 1))
			return r, body
		}, v, ErrMissing},
		{"garbled signature", func() (*http.Request, []byte) {
			r := signed(t, "POST", "/api/jobs", body, good)
			r.Header.Set("Signature", "paradox=:bm90IGEgc2lnbmF0dXJl:")
			return r, body
		}, v, ErrMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, b := c.req()
			if _, err := c.v.Verify(r, b); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestWindowEdgesAreAccepted(t *testing.T) {
	pub, priv := newKey(t)
	for _, skew := range []time.Duration{-5 * time.Minute, 5 * time.Minute} {
		r := signed(t, "GET", "/api/jobs/42", nil, Signer{KeyID: "citadel", Key: priv, Direction: ToParadox, Now: fixed(t0.Add(skew))})
		if _, err := verifierFor("citadel", ToParadox, pub).Verify(r, nil); err != nil {
			t.Fatalf("skew %v refused: %v", skew, err)
		}
	}
}

func TestTwoKeysDuringAChange(t *testing.T) {
	oldPub, oldPriv := newKey(t)
	newPub, newPriv := newKey(t)
	v := verifierFor("citadel", ToParadox, oldPub, newPub)
	for i, priv := range []ed25519.PrivateKey{oldPriv, newPriv} {
		r := signed(t, "POST", "/api/jobs", nil, Signer{KeyID: "citadel", Key: priv, Direction: ToParadox, Now: fixed(t0)})
		res, err := v.Verify(r, nil)
		want := Fingerprint([]ed25519.PublicKey{oldPub, newPub}[i])
		if err != nil || res.Fingerprint != want {
			t.Fatalf("key %d: %+v, %v", i, res, err)
		}
	}
}

// The signature base is part of the published contract: if this test has
// to change, the contract has a breaking change (PX-CON-02).
func TestSignatureBaseIsStable(t *testing.T) {
	got := string(signatureBase("post", "/jobs/send-reminders", contentDigest(nil),
		signatureParams(1793496600, ParadoxKeyID, ToSystem)))
	want := `"@method": POST
"@path": /jobs/send-reminders
"content-digest": sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:
"@signature-params": ("@method" "@path" "content-digest");created=1793496600;keyid="paradox";alg="ed25519";tag="paradox-to-system"`
	if got != want {
		t.Fatalf("signature base changed:\n%s\nwant:\n%s", got, want)
	}
}
