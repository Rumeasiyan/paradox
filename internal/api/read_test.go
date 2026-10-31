package api

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Rumeasiyan/paradox/sign"
)

// queueRun queues a run with data for citadel through the API and
// returns its ID.
func queueRun(t *testing.T, e *env, requestID string) string {
	t.Helper()
	code, out := e.call("citadel", "POST", Prefix, queue("generate-certificates", requestID, map[string]any{"data": map[string]any{"secret": "course-secret-42"}}), nil)
	if code != http.StatusAccepted {
		t.Fatalf("queue: %d %v", code, out)
	}
	return out["run_id"].(string)
}

// PX-JOB-04: a freshly queued run reads as queued, with no attempts and
// null times, and the answer has exactly the contract's keys.
func TestReadQueuedRun(t *testing.T) {
	e := newEnv(t)
	runID := queueRun(t, e, "read-queued")
	code, out := e.call("citadel", "GET", Prefix+"/"+runID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	var keys []string
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "attempts,due_at,finished_at,job,run_id,started_at,state"; got != want {
		t.Fatalf("keys %s, want %s", got, want)
	}
	if out["run_id"] != runID || out["job"] != "generate-certificates" || out["state"] != "queued" ||
		out["attempts"] != float64(0) || out["started_at"] != nil || out["finished_at"] != nil ||
		out["due_at"] != e.now.Format(time.RFC3339) {
		t.Fatalf("%v", out)
	}
}

// Times stored with any offset read back in UTC.
func TestReadFinishedRunInUTC(t *testing.T) {
	e := newEnv(t)
	runID := queueRun(t, e, "read-finished")
	_, err := e.pool.Exec(context.Background(), `
		UPDATE run SET state = 'succeeded', attempts = 1, data = NULL,
			started_at = '2026-11-01T22:00:00+05:30', finished_at = '2026-11-01T22:01:30.5+05:30'
		WHERE id = $1`, runID)
	if err != nil {
		t.Fatal(err)
	}
	code, out := e.call("citadel", "GET", Prefix+"/"+runID, nil, nil)
	if code != http.StatusOK || out["state"] != "succeeded" || out["attempts"] != float64(1) ||
		out["started_at"] != "2026-11-01T16:30:00Z" || out["finished_at"] != "2026-11-01T16:31:30.5Z" {
		t.Fatalf("%d %v", code, out)
	}
	due, _ := out["due_at"].(string)
	if !strings.HasSuffix(due, "Z") {
		t.Fatalf("due_at %q not UTC", due)
	}
}

// PX-SIG-05: a system cannot read another system's run, and the refusal
// carries no run fields.
func TestReadOtherSystemsRunIsRefused(t *testing.T) {
	e := newEnv(t)
	runID := queueRun(t, e, "read-other")
	code, out := e.call("website", "GET", Prefix+"/"+runID, nil, nil)
	if code != http.StatusForbidden || out["error"] != "not_allowed" || out["state"] != nil || out["job"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	if !strings.Contains(e.logs.String(), `"system":"website"`) {
		t.Fatalf("refusal not logged with the system:\n%s", e.logs.String())
	}
}

func TestReadUnknownRunIsNotFound(t *testing.T) {
	e := newEnv(t)
	queueRun(t, e, "read-unknown")
	for name, id := range map[string]string{
		"unknown UUID": "00000000-0000-4000-8000-000000000000",
		"not a UUID":   "not-a-run",
		"upper case":   "00000000-0000-4000-8000-00000000000A",
	} {
		t.Run(name, func(t *testing.T) {
			code, out := e.call("citadel", "GET", Prefix+"/"+id, nil, nil)
			if code != http.StatusNotFound || out["error"] != "not_found" {
				t.Fatalf("%d %v", code, out)
			}
		})
	}
}

// The read route sits behind the signature check.
func TestReadNeedsSignature(t *testing.T) {
	e := newEnv(t)
	runID := queueRun(t, e, "read-unsigned")
	code, out := e.call("", "GET", Prefix+"/"+runID, nil, nil)
	if code != http.StatusUnauthorized || out["error"] != "not_authenticated" || out["state"] != nil {
		t.Fatalf("%d %v", code, out)
	}
}

// PX-NFR-06: reading a run never returns its data, even while it is
// still stored.
func TestReadNeverReturnsData(t *testing.T) {
	e := newEnv(t)
	runID := queueRun(t, e, "read-data")
	req, _ := http.NewRequest("GET", e.srv.URL+Prefix+"/"+runID, nil)
	s := sign.Signer{KeyID: "citadel", Key: e.keys["citadel"], Direction: sign.ToParadox, Now: func() time.Time { return e.now }}
	if err := s.Sign(req, nil); err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(b), "course-secret-42") || strings.Contains(string(b), `"data"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", resp.Header)
	}
}
