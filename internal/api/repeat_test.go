package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// counts returns the number of runs and River jobs in the database.
func (e *env) counts() (runs, riverJobs int) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM run`).Scan(&runs); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&riverJobs); err != nil {
		e.t.Fatal(err)
	}
	return runs, riverJobs
}

// PX-JOB-03: a repeat within the keep returns the first run and queues
// nothing new.
func TestRepeatReturnsFirstRun(t *testing.T) {
	e := newEnv(t)
	body := queue("generate-certificates", "course-42.publish", map[string]any{"data": map[string]any{"course_id": 42}})
	code, first := e.call("citadel", "POST", Prefix, body, nil)
	if code != http.StatusAccepted || first["repeat"] != false {
		t.Fatalf("first: %d %v", code, first)
	}
	e.now = e.now.Add(time.Hour)
	code, again := e.call("citadel", "POST", Prefix, body, nil)
	if code != http.StatusAccepted || again["repeat"] != true || again["run_id"] != first["run_id"] ||
		again["job"] != "generate-certificates" || again["state"] != "queued" || again["due_at"] != first["due_at"] {
		t.Fatalf("repeat: %d %v, first %v", code, again, first)
	}
	if runs, jobs := e.counts(); runs != 1 || jobs != 1 {
		t.Fatalf("%d runs and %d River jobs after a repeat, want 1 and 1", runs, jobs)
	}
}

// The repeat shows the run as it is now, not as it was first answered.
func TestRepeatShowsCurrentState(t *testing.T) {
	e := newEnv(t)
	body := queue("generate-certificates", "state-check", nil)
	_, first := e.call("citadel", "POST", Prefix, body, nil)
	if _, err := e.pool.Exec(context.Background(), `
		UPDATE run SET state = 'succeeded', started_at = $2, finished_at = $2 WHERE id = $1`,
		first["run_id"], e.now); err != nil {
		t.Fatal(err)
	}
	code, again := e.call("citadel", "POST", Prefix, body, nil)
	if code != http.StatusAccepted || again["repeat"] != true || again["run_id"] != first["run_id"] || again["state"] != "succeeded" {
		t.Fatalf("%d %v", code, again)
	}
}

// The first run is returned even when the repeat names another job and
// other data (contract section 6).
func TestRepeatNamingAnotherJobReturnsFirstRun(t *testing.T) {
	e := newEnv(t)
	_, first := e.call("citadel", "POST", Prefix, queue("generate-certificates", "same-id", map[string]any{"data": 1}), nil)
	code, again := e.call("citadel", "POST", Prefix, queue("daily", "same-id", map[string]any{"data": 2}), nil)
	if code != http.StatusAccepted || again["repeat"] != true || again["run_id"] != first["run_id"] || again["job"] != "generate-certificates" {
		t.Fatalf("%d %v", code, again)
	}
	if runs, jobs := e.counts(); runs != 1 || jobs != 1 {
		t.Fatalf("%d runs and %d River jobs, want 1 and 1", runs, jobs)
	}
}

// After PARADOX_REQUEST_ID_KEEP the request ID is forgotten: a new run is
// queued and the first run stays.
func TestRepeatAfterKeepQueuesNewRun(t *testing.T) {
	e := newEnv(t)
	body := queue("generate-certificates", "nightly", nil)
	_, first := e.call("citadel", "POST", Prefix, body, nil)
	e.now = e.now.Add(25 * time.Hour)
	code, again := e.call("citadel", "POST", Prefix, body, nil)
	if code != http.StatusAccepted || again["repeat"] != false || again["run_id"] == first["run_id"] || again["state"] != "queued" {
		t.Fatalf("%d %v, first %v", code, again, first)
	}
	if runs, jobs := e.counts(); runs != 2 || jobs != 2 {
		t.Fatalf("%d runs and %d River jobs, want 2 and 2", runs, jobs)
	}
	var mapped string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT q.run_id::text FROM queue_request q JOIN system s ON s.id = q.system_id
		WHERE s.name = 'citadel' AND q.request_id = 'nightly'`).Scan(&mapped); err != nil || mapped != again["run_id"] {
		t.Fatalf("request ID maps to %q (%v), want the new run %v", mapped, err, again["run_id"])
	}
	// A repeat now finds the new run.
	code, third := e.call("citadel", "POST", Prefix, body, nil)
	if code != http.StatusAccepted || third["repeat"] != true || third["run_id"] != again["run_id"] {
		t.Fatalf("%d %v", code, third)
	}
}

// Request IDs belong to one system: the same ID from another system is a
// new request.
func TestRepeatIsPerSystem(t *testing.T) {
	e := newEnv(t)
	_, a := e.call("citadel", "POST", Prefix, queue("generate-certificates", "shared-id", nil), nil)
	code, b := e.call("website", "POST", Prefix, queue("rebuild", "shared-id", nil), nil)
	if code != http.StatusAccepted || b["repeat"] != false || b["run_id"] == a["run_id"] || b["job"] != "rebuild" {
		t.Fatalf("%d %v, citadel %v", code, b, a)
	}
	if runs, _ := e.counts(); runs != 2 {
		t.Fatalf("%d runs, want 2", runs)
	}
}

// Identical requests at the same moment give one run, and every one of
// them is answered 202 with that run.
func TestConcurrentRepeatsGiveOneRun(t *testing.T) {
	e := newEnv(t)
	const n = 8
	body := queue("generate-certificates", "burst", nil)
	type result struct {
		code int
		out  map[string]any
	}
	results := make([]result, n)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range n {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			code, out := e.call("citadel", "POST", Prefix, body, nil)
			results[i] = result{code, out}
		}()
	}
	start.Done()
	done.Wait()
	firsts := 0
	runID := results[0].out["run_id"]
	for i, r := range results {
		if r.code != http.StatusAccepted || r.out["run_id"] != runID {
			t.Fatalf("request %d: %d %v, want 202 for run %v", i, r.code, r.out, runID)
		}
		if r.out["repeat"] == false {
			firsts++
		}
	}
	if firsts != 1 {
		t.Fatalf("%d answers with repeat false, want 1", firsts)
	}
	if runs, jobs := e.counts(); runs != 1 || jobs != 1 {
		t.Fatalf("%d runs and %d River jobs, want 1 and 1", runs, jobs)
	}
}
