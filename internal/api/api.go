// Package api is the queue API systems call (contract section 6): queue a
// job, read a run, cancel a run. Every request is signed by the calling
// system and checked before anything else happens (PX-SIG-02 to PX-SIG-05);
// a system may only touch its own jobs and runs.
package api

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/Rumeasiyan/paradox/sign"
)

// Prefix is the only path the API serves; a deployment may expose just
// this to the internet.
const Prefix = "/api/jobs"

// envelopeBytes is room for the request's own fields around the data.
const envelopeBytes = 8 << 10

// API serves the queue API.
type API struct {
	Pool          *pgxpool.Pool
	River         *river.Client[pgx.Tx]
	Keys          func(system string) []ed25519.PublicKey // current systems' public keys
	Window        time.Duration                           // PARADOX_SIGNATURE_WINDOW
	MaxDataBytes  int                                     // PARADOX_MAX_DATA_BYTES
	MaxAhead      time.Duration                           // PARADOX_MAX_AHEAD
	RequestIDKeep time.Duration                           // PARADOX_REQUEST_ID_KEEP
	Logger        *slog.Logger
	Now           func() time.Time
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// caller is a request whose signature checked out.
type caller struct {
	system      string
	fingerprint string
	body        []byte
}

// Handler returns the API's routes, each behind the signature check.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Prefix, a.signed(a.create))
	mux.HandleFunc("GET "+Prefix+"/{run_id}", a.signed(a.read))
	mux.HandleFunc("POST "+Prefix+"/{run_id}/cancel", a.signed(a.cancel))
	mux.HandleFunc(Prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such API path")
	})
	return mux
}

// signed reads the body and checks the signature before the route runs.
// A refusal is logged with the claimed system and the reason, never the
// body (PX-SIG-05, PX-NFR-06).
func (a *API) signed(next func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(a.MaxDataBytes+envelopeBytes)))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body is over the limit")
				return
			}
			writeError(w, http.StatusBadRequest, "bad_request", "request body could not be read")
			return
		}
		res, err := sign.Verifier{Keys: a.Keys, Direction: sign.ToParadox, Window: a.Window, Now: a.Now}.Verify(r, body)
		if err != nil {
			a.Logger.Warn("request refused", "reason", err.Error(), "path", r.URL.Path, "claimed_system", claimedKeyID(r))
			writeError(w, http.StatusUnauthorized, "not_authenticated", "signature missing, invalid, expired or from an unknown system")
			return
		}
		next(w, r, caller{system: res.KeyID, fingerprint: res.Fingerprint, body: body})
	}
}

// refuse answers 403 for a job or run that is not the caller's, and logs it.
func (a *API) refuse(w http.ResponseWriter, c caller, why string) {
	a.Logger.Warn("request refused", "reason", why, "system", c.system)
	writeError(w, http.StatusForbidden, "not_allowed", why)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

// claimedKeyID pulls the key ID out of Signature-Input to log a refusal.
// It is only what the caller claims, not proof, and is cut to 63 bytes.
func claimedKeyID(r *http.Request) string {
	_, rest, ok := strings.Cut(r.Header.Get("Signature-Input"), `keyid="`)
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, `"`)
	if len(id) > 63 {
		id = id[:63]
	}
	return id
}
