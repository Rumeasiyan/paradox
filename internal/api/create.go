package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Rumeasiyan/paradox/internal/runner"
)

// requestIDRE is the contract's rule for request_id.
var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type createRequest struct {
	Job       string          `json:"job"`
	Data      json.RawMessage `json:"data"`
	RunAt     *string         `json:"run_at"`
	RequestID string          `json:"request_id"`
}

type createAnswer struct {
	RunID  string    `json:"run_id"`
	Job    string    `json:"job"`
	State  string    `json:"state"`
	DueAt  time.Time `json:"due_at"`
	Repeat bool      `json:"repeat"`
}

// create queues one of the caller's own jobs (PX-JOB-01, PX-JOB-02;
// how it works, Flow B).
func (a *API) create(w http.ResponseWriter, r *http.Request, c caller) {
	var req createRequest
	dec := json.NewDecoder(bytes.NewReader(c.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "body is not a valid queue request: "+err.Error())
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "bad_request", "body holds more than one JSON value")
		return
	}
	if req.Job == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "job is required")
		return
	}
	if !requestIDRE.MatchString(req.RequestID) {
		writeError(w, http.StatusBadRequest, "bad_request", "request_id must be 1 to 128 characters from A-Z a-z 0-9 . _ -")
		return
	}
	data := []byte(req.Data)
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		data = nil
	}
	if len(data) > a.MaxDataBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("data is over %d bytes", a.MaxDataBytes))
		return
	}
	now := a.now()
	due := now
	if req.RunAt != nil {
		t, err := time.Parse(time.RFC3339, *req.RunAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "run_at must be RFC 3339 with an offset, for example 2026-11-01T22:00:00+05:30")
			return
		}
		if t.After(now.Add(a.MaxAhead)) {
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("run_at is more than %s ahead", a.MaxAhead))
			return
		}
		if t.After(now) {
			due = t
		}
	}

	var ans createAnswer
	err := pgx.BeginFunc(r.Context(), a.Pool, func(tx pgx.Tx) error {
		var jobID, systemID int64
		var timeoutS, retries int
		err := tx.QueryRow(r.Context(), `
			SELECT j.id, s.id, j.timeout_s, j.retries
			FROM job j JOIN system s ON s.id = j.system_id
			WHERE s.name = $1 AND j.name = $2 AND s.removed_at IS NULL`, c.system, req.Job).
			Scan(&jobID, &systemID, &timeoutS, &retries)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotOwnJob
		}
		if err != nil {
			return err
		}
		if prev, ok, err := a.repeatOf(r.Context(), tx, systemID, req.RequestID, now); err != nil {
			return err
		} else if ok {
			ans = prev
			ans.Repeat = true
			return nil
		}
		runID, err := runner.Enqueue(r.Context(), tx, a.River, jobID, "queue", data, due, timeoutS, retries+1)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO queue_request (system_id, request_id, run_id, received_at, key_fingerprint)
			VALUES ($1, $2, $3, $4, $5)`, systemID, req.RequestID, runID, now, c.fingerprint); err != nil {
			return err
		}
		ans = createAnswer{RunID: runID, Job: req.Job, State: "queued", DueAt: due.UTC()}
		return nil
	})
	switch {
	case errors.Is(err, errNotOwnJob):
		a.refuse(w, c, "job is not in this system's schedule file")
	case err != nil:
		a.Logger.Error("queue request failed", "system", c.system, "job", req.Job, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "the job could not be queued; try again")
	default:
		a.Logger.Info("job queued", "system", c.system, "job", req.Job, "run_id", ans.RunID, "repeat", ans.Repeat)
		writeJSON(w, http.StatusAccepted, ans)
	}
}

var errNotOwnJob = errors.New("not the caller's job")
