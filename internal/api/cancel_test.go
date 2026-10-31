package api

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// queueRun queues one of the system's jobs with some data and returns the
// run ID.
func (e *env) queueRun(system, job, requestID string) string {
	e.t.Helper()
	code, out := e.call(system, "POST", Prefix, queue(job, requestID, map[string]any{"data": map[string]any{"course_id": 42}}), nil)
	if code != http.StatusAccepted {
		e.t.Fatalf("queue: %d %v", code, out)
	}
	return out["run_id"].(string)
}

func cancelPath(runID string) string { return Prefix + "/" + runID + "/cancel" }

// PX-JOB-05, PX-REC-02: a queued run is cancelled, its data wiped, the
// action recorded and its River job cancelled.
func TestCancelQueuedRun(t *testing.T) {
	e := newEnv(t)
	runID := e.queueRun("citadel", "generate-certificates", "course-42.publish")
	e.now = e.now.Add(time.Minute)

	code, out := e.call("citadel", "POST", cancelPath(runID), nil, nil)
	if code != http.StatusOK || out["state"] != "cancelled" || out["run_id"] != runID || out["job"] != "generate-certificates" || out["finished_at"] != e.now.Format(time.RFC3339) {
		t.Fatalf("%d %v", code, out)
	}

	ctx := context.Background()
	var state string
	var hasData bool
	var finished *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT state, data IS NOT NULL, finished_at FROM run WHERE id = $1`, runID).Scan(&state, &hasData, &finished); err != nil {
		t.Fatal(err)
	}
	if state != "cancelled" || hasData || finished == nil || !finished.Equal(e.now) {
		t.Fatalf("stored %s data=%v finished=%v", state, hasData, finished)
	}

	var actions int
	var by string
	var acted time.Time
	if err := e.pool.QueryRow(ctx, `
		SELECT count(*) OVER (), s.name, a.acted_at
		FROM run_action a JOIN system s ON s.id = a.by_system_id
		WHERE a.run_id = $1 AND a.action = 'cancel'`, runID).Scan(&actions, &by, &acted); err != nil {
		t.Fatal(err)
	}
	if actions != 1 || by != "citadel" || !acted.Equal(e.now) {
		t.Fatalf("run_action: %d by %s at %v", actions, by, acted)
	}

	var riverState string
	if err := e.pool.QueryRow(ctx, `SELECT state FROM river_job WHERE args->>'run_id' = $1`, runID).Scan(&riverState); err != nil {
		t.Fatal(err)
	}
	if riverState != "cancelled" {
		t.Fatalf("River job is %s", riverState)
	}

	code, out = e.call("citadel", "POST", cancelPath(runID), nil, nil)
	if code != http.StatusConflict || out["error"] != "conflict" {
		t.Fatalf("second cancel: %d %v", code, out)
	}
	e.pool.QueryRow(ctx, `SELECT count(*) FROM run_action WHERE run_id = $1`, runID).Scan(&actions)
	if actions != 1 {
		t.Fatalf("%d actions after a refused cancel", actions)
	}
}

func TestCancelRunThatLeftTheQueue(t *testing.T) {
	e := newEnv(t)
	for _, state := range []string{"running", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			runID := e.queueRun("citadel", "generate-certificates", "left-"+state)
			if _, err := e.pool.Exec(context.Background(), `UPDATE run SET state = $2 WHERE id = $1`, runID, state); err != nil {
				t.Fatal(err)
			}
			code, out := e.call("citadel", "POST", cancelPath(runID), nil, nil)
			if code != http.StatusConflict || out["error"] != "conflict" {
				t.Fatalf("%d %v", code, out)
			}
			var got string
			var hasData bool
			e.pool.QueryRow(context.Background(), `SELECT state, data IS NOT NULL FROM run WHERE id = $1`, runID).Scan(&got, &hasData)
			if got != state || !hasData {
				t.Fatalf("run changed to %s data=%v", got, hasData)
			}
		})
	}
}

// PX-SIG-05: a system cannot cancel another system's run.
func TestCancelOtherSystemsRun(t *testing.T) {
	e := newEnv(t)
	runID := e.queueRun("website", "rebuild", "nightly")
	code, out := e.call("citadel", "POST", cancelPath(runID), nil, nil)
	if code != http.StatusForbidden || out["error"] != "not_allowed" {
		t.Fatalf("%d %v", code, out)
	}
	ctx := context.Background()
	var state, riverState string
	var actions int
	e.pool.QueryRow(ctx, `SELECT state FROM run WHERE id = $1`, runID).Scan(&state)
	e.pool.QueryRow(ctx, `SELECT state FROM river_job WHERE args->>'run_id' = $1`, runID).Scan(&riverState)
	e.pool.QueryRow(ctx, `SELECT count(*) FROM run_action`).Scan(&actions)
	if state != "queued" || riverState == "cancelled" || actions != 0 {
		t.Fatalf("run %s, River job %s, %d actions", state, riverState, actions)
	}
}

func TestCancelRefusals(t *testing.T) {
	e := newEnv(t)
	runID := e.queueRun("citadel", "generate-certificates", "refusals")
	for name, c := range map[string]struct {
		system, path string
		body         any
		want         int
		code         string
	}{
		"unknown run":    {"citadel", cancelPath("00000000-0000-4000-8000-000000000000"), nil, http.StatusNotFound, "not_found"},
		"not a run ID":   {"citadel", cancelPath("nope"), nil, http.StatusNotFound, "not_found"},
		"non-empty body": {"citadel", cancelPath(runID), map[string]any{"reason": "changed my mind"}, http.StatusBadRequest, "bad_request"},
		"unsigned":       {"", cancelPath(runID), nil, http.StatusUnauthorized, "not_authenticated"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out := e.call(c.system, "POST", c.path, c.body, nil)
			if code != c.want || out["error"] != c.code {
				t.Fatalf("%d %v, want %d", code, out, c.want)
			}
		})
	}
	var state string
	e.pool.QueryRow(context.Background(), `SELECT state FROM run WHERE id = $1`, runID).Scan(&state)
	if state != "queued" {
		t.Fatalf("run is %s after refused cancels", state)
	}
}
