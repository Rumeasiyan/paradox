// Package sign signs and checks the HTTP calls between PARADOX and the
// systems it serves, in both directions (PX-SIG-01 to PX-SIG-04).
//
// The wire format is a fixed profile of HTTP Message Signatures (RFC 9421)
// with Content-Digest (RFC 9530), so other languages can use a standard
// library. Every signature covers the method, the path and a SHA-256 digest
// of the body, and carries its creation time, the signer's key ID, the
// algorithm, and the direction as the RFC's tag parameter:
//
//	Content-Digest: sha-256=:<base64>:
//	Signature-Input: paradox=("@method" "@path" "content-digest");created=<unix>;keyid="<id>";alg="ed25519";tag="<direction>"
//	Signature: paradox=:<base64>:
//
// The key ID is "paradox" for calls from PARADOX, and the system's name for
// calls from a system.
//
// Systems normally need two calls: VerifyFromParadox on their job address,
// and SignForParadox before queueing a job.
package sign

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Direction says which way a call goes; it is part of what is signed, so a
// signature made for one direction is refused in the other (PX-SIG-04).
type Direction string

const (
	ToSystem  Direction = "paradox-to-system"
	ToParadox Direction = "system-to-paradox"
)

// ParadoxKeyID is the key ID PARADOX signs with.
const ParadoxKeyID = "paradox"

// DefaultWindow is how far a signature's time may be from the receiver's
// clock, either way (PX-SIG-03).
const DefaultWindow = 5 * time.Minute

const (
	label      = "paradox"
	components = `("@method" "@path" "content-digest")`
)

// Errors returned by Verify. All of them mean "not authenticated".
var (
	ErrMissing        = errors.New("signature headers missing or malformed")
	ErrDigest         = errors.New("body does not match Content-Digest")
	ErrWrongDirection = errors.New("signature made for the other direction")
	ErrUnknownKey     = errors.New("unknown key ID")
	ErrExpired        = errors.New("signature time outside the allowed window")
	ErrBadSignature   = errors.New("signature does not verify")
)

// Signer adds signature headers to outgoing requests.
type Signer struct {
	KeyID     string
	Key       ed25519.PrivateKey
	Direction Direction
	Now       func() time.Time // nil means time.Now
}

// Sign sets Content-Digest, Signature-Input and Signature on r. body must
// be exactly the bytes that will be sent.
func (s Signer) Sign(r *http.Request, body []byte) error {
	if s.KeyID == "" || strings.ContainsAny(s.KeyID, "\"\\") {
		return errors.New("sign: key ID must be set and must not contain quotes")
	}
	if len(s.Key) != ed25519.PrivateKeySize {
		return errors.New("sign: private key missing")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	digest := contentDigest(body)
	params := signatureParams(now().Unix(), s.KeyID, s.Direction)
	sig := ed25519.Sign(s.Key, signatureBase(r.Method, path(r), digest, params))
	r.Header.Set("Content-Digest", digest)
	r.Header.Set("Signature-Input", label+"="+params)
	r.Header.Set("Signature", label+"=:"+base64.StdEncoding.EncodeToString(sig)+":")
	return nil
}

// Verifier checks incoming requests.
type Verifier struct {
	// Keys returns the public keys that may sign as keyID: one, or two while
	// a key is being changed (PX-SIG-06). None means the key ID is unknown.
	Keys      func(keyID string) []ed25519.PublicKey
	Direction Direction
	Window    time.Duration    // zero means DefaultWindow
	Now       func() time.Time // nil means time.Now
}

// Result says who signed a verified request.
type Result struct {
	KeyID       string // the signer: "paradox" or a system name
	Fingerprint string // which of the signer's keys matched
}

// Verify checks r's signature against body, which must be the full request
// body as received. Any error means the request must be refused.
func (v Verifier) Verify(r *http.Request, body []byte) (Result, error) {
	in, err := parseInput(r.Header.Get("Signature-Input"))
	if err != nil {
		return Result{}, err
	}
	sig, err := parseSignature(r.Header.Get("Signature"))
	if err != nil {
		return Result{}, err
	}
	if in.direction != v.Direction {
		return Result{}, ErrWrongDirection
	}
	digest := contentDigest(body)
	if r.Header.Get("Content-Digest") != digest {
		return Result{}, ErrDigest
	}
	window := v.Window
	if window == 0 {
		window = DefaultWindow
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	if age := now().Sub(time.Unix(in.created, 0)); age > window || age < -window {
		return Result{}, ErrExpired
	}
	keys := v.Keys(in.keyID)
	if len(keys) == 0 {
		return Result{}, ErrUnknownKey
	}
	base := signatureBase(r.Method, path(r), digest, signatureParams(in.created, in.keyID, in.direction))
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, base, sig) {
			return Result{KeyID: in.keyID, Fingerprint: Fingerprint(k)}, nil
		}
	}
	return Result{}, ErrBadSignature
}

// SignForParadox signs a request a system sends to PARADOX, such as
// queueing a job.
func SignForParadox(r *http.Request, body []byte, system string, key ed25519.PrivateKey) error {
	return Signer{KeyID: system, Key: key, Direction: ToParadox}.Sign(r, body)
}

// VerifyFromParadox checks that a call to a system's job address really
// comes from PARADOX. paradoxKeys holds PARADOX's public key, or two while
// it is being changed.
func VerifyFromParadox(r *http.Request, body []byte, paradoxKeys ...ed25519.PublicKey) error {
	_, err := Verifier{
		Direction: ToSystem,
		Keys: func(id string) []ed25519.PublicKey {
			if id == ParadoxKeyID {
				return paradoxKeys
			}
			return nil
		},
	}.Verify(r, body)
	return err
}

func contentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
}

func signatureParams(created int64, keyID string, d Direction) string {
	return fmt.Sprintf(`%s;created=%d;keyid="%s";alg="ed25519";tag="%s"`, components, created, keyID, string(d))
}

// signatureBase is the RFC 9421 signature base for the fixed component list.
func signatureBase(method, path, digest, params string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "\"@method\": %s\n", strings.ToUpper(method))
	fmt.Fprintf(&b, "\"@path\": %s\n", path)
	fmt.Fprintf(&b, "\"content-digest\": %s\n", digest)
	fmt.Fprintf(&b, "\"@signature-params\": %s", params)
	return b.Bytes()
}

func path(r *http.Request) string {
	if p := r.URL.EscapedPath(); p != "" {
		return p
	}
	return "/"
}

type input struct {
	created   int64
	keyID     string
	direction Direction
}

// parseInput accepts only the exact profile this package writes. Being
// strict keeps both sides' signature bases byte-for-byte identical.
func parseInput(h string) (input, error) {
	rest, ok := strings.CutPrefix(h, label+"="+components+";created=")
	if !ok {
		return input{}, ErrMissing
	}
	num, rest, ok := strings.Cut(rest, `;keyid="`)
	if !ok {
		return input{}, ErrMissing
	}
	created, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return input{}, ErrMissing
	}
	keyID, rest, ok := strings.Cut(rest, `";alg="ed25519";tag="`)
	if !ok || keyID == "" || strings.ContainsAny(keyID, "\"\\") {
		return input{}, ErrMissing
	}
	tag, ok := strings.CutSuffix(rest, `"`)
	if !ok || strings.ContainsAny(tag, "\"\\") {
		return input{}, ErrMissing
	}
	in := input{created: created, keyID: keyID, direction: Direction(tag)}
	if signatureParams(in.created, in.keyID, in.direction) != strings.TrimPrefix(h, label+"=") {
		return input{}, ErrMissing
	}
	return in, nil
}

func parseSignature(h string) ([]byte, error) {
	rest, ok := strings.CutPrefix(h, label+"=:")
	if !ok {
		return nil, ErrMissing
	}
	b64, ok := strings.CutSuffix(rest, ":")
	if !ok {
		return nil, ErrMissing
	}
	sig, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrMissing
	}
	return sig, nil
}
