package runner

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/Rumeasiyan/paradox/internal/db"
	"github.com/Rumeasiyan/paradox/internal/schedule"
)

// Story 28 (E-RUN-04): clean shutdown, and runs left unfinished by a
// shutdown or a crash are retried after the restart (PX-RUN-05, issue #8).
// Each test stops the harness's River client the way serve does
// (db.StopRiver), then starts a new client on the same database, standing
// in for the restarted PARADOX.

// restart starts a new River client with the runner worker on the
// harness's database and dependencies.
func (h *harness) restart() *river.Client[pgx.Tx] {
	h.t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &Worker{D: h.sched.D})
	cfg := db.RiverConfig(workers, harnessMaxRunning)
	cfg.Logger = slog.New(slog.DiscardHandler)
	rc, err := river.NewClient(riverpgxv5.New(h.pool), cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := rc.Start(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rc.Stop(stop)
	})
	return rc
}

// waitRunning polls until the run's first attempt has reached the system.
func (h *harness) waitRunning(runID string) {
	h.t.Helper()
	for end := time.Now().Add(10 * time.Second); ; {
		var state string
		h.pool.QueryRow(context.Background(), `SELECT state FROM run WHERE id = $1`, runID).Scan(&state)
		if state == "running" && h.hits.Load() > 0 {
			return
		}
		if time.Now().After(end) {
			h.t.Fatalf("run still %s, %d calls, 10s after it was due", state, h.hits.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// snapshot reads a run as it stands, without waiting.
func (h *harness) snapshot(runID string) runRow {
	h.t.Helper()
	var r runRow
	var lastErr *string
	err := h.pool.QueryRow(context.Background(), `
		SELECT state, attempts, last_error, data IS NOT NULL, finished_at IS NOT NULL FROM run WHERE id = $1`, runID).
		Scan(&r.state, &r.attempts, &lastErr, &r.hasData, &r.finished)
	if err != nil {
		h.t.Fatal(err)
	}
	if lastErr != nil {
		r.lastError = *lastErr
	}
	return r
}

// attemptLog lists the run's attempts as "number outcome: error", with
// "open" for an attempt that has no end.
func (h *harness) attemptLog(runID string) []string {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(), `
		SELECT number || ' ' || CASE WHEN ended_at IS NULL THEN 'open' ELSE outcome || ': ' || coalesce(error, '-') END
		FROM attempt WHERE run_id = $1 ORDER BY number`, runID)
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *harness) riverJob(runID string) (state string, attempt, maxAttempts int) {
	h.t.Helper()
	if err := h.pool.QueryRow(context.Background(), `
		SELECT state, attempt, max_attempts FROM river_job WHERE args->>'run_id' = $1`, runID).
		Scan(&state, &attempt, &maxAttempts); err != nil {
		h.t.Fatal(err)
	}
	return state, attempt, maxAttempts
}

// PX-RUN-05: an attempt that outlasts PARADOX_SHUTDOWN_GRACE is cut off,
// recorded truthfully as interrupted by the shutdown, and its run goes back
// to the queue with its data. After the restart it is retried and succeeds.
func TestShutdownDuringASlowAttemptLeavesTheRunQueuedAndItSucceedsAfterRestart(t *testing.T) {
	h := newHarness(t, nil, map[string]time.Duration{"slow": 2 * time.Second}, func(d *Deps) {
		d.RetryDelays = []time.Duration{100 * time.Millisecond}
	}, jobSpec{"citadel", schedule.WakeSablier, "slow", 10, 1})
	id := h.due("slow")
	h.waitRunning(id)

	const grace = 300 * time.Millisecond
	began := time.Now()
	if err := db.StopRiver(h.sched.River, grace); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if took := time.Since(began); took < grace || took > grace+time.Second {
		t.Fatalf("stop took %v; want the grace of %v and a moment to record", took, grace)
	}
	if r := h.snapshot(id); r.state != "queued" || r.attempts != 1 || r.finished || r.lastError != "interrupted by shutdown" {
		t.Fatalf("after shutdown: run %+v", r)
	}
	if got := h.attemptLog(id); strings.Join(got, ",") != "1 error: interrupted by shutdown" {
		t.Fatalf("after shutdown: attempts %v", got)
	}
	// River makes a retry this close at hand available at once.
	if state, _, _ := h.riverJob(id); state != "retryable" && state != "available" {
		t.Fatalf("after shutdown: River job %s, want retryable or available", state)
	}

	h.restart()
	r := h.wait(id, 20*time.Second)
	if r.state != "succeeded" || r.attempts != 2 || r.hasData || !r.finished {
		t.Fatalf("after restart: run %+v", r)
	}
	if got := h.attemptLog(id); strings.Join(got, ",") != "1 error: interrupted by shutdown,2 success: -" {
		t.Fatalf("after restart: attempts %v", got)
	}
	if n := h.hits.Load(); n != 2 {
		t.Fatalf("system called %d times, want 2", n)
	}
}

// A shutdown that cuts off a run's last attempt fails the run, wipes its
// data and reports it once, like any final failure (PX-REP-02, PX-REC-02).
func TestShutdownDuringTheLastAttemptFailsAndReportsTheRun(t *testing.T) {
	rep := &fakeReporter{}
	h := newHarness(t, nil, map[string]time.Duration{"slow": 2 * time.Second}, func(d *Deps) {
		d.Reporter = rep
	}, jobSpec{"citadel", schedule.WakeNone, "slow", 10, 0})
	id := h.due("slow")
	h.waitRunning(id)
	// The worker keeps River's own limit one above the attempt in progress,
	// so that River's rescuer would retry the job after a crash rather than
	// discard it unseen.
	if _, attempt, maxAttempts := h.riverJob(id); attempt != 1 || maxAttempts != 2 {
		t.Fatalf("River job attempt %d of %d during the last attempt, want 1 of 2", attempt, maxAttempts)
	}

	if err := db.StopRiver(h.sched.River, 200*time.Millisecond); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if r := h.snapshot(id); r.state != "failed" || r.attempts != 1 || r.hasData || !r.finished || r.lastError != "interrupted by shutdown" {
		t.Fatalf("after shutdown: run %+v", r)
	}
	if got := h.attemptLog(id); strings.Join(got, ",") != "1 error: interrupted by shutdown" {
		t.Fatalf("attempts %v", got)
	}
	if fs := rep.list(); len(fs) != 1 || fs[0].RunID != id || fs[0].LastError != "interrupted by shutdown" {
		t.Fatalf("reports %+v, want one", fs)
	}
	if state, _, _ := h.riverJob(id); state != "cancelled" {
		t.Fatalf("River job %s, want cancelled", state)
	}
}

// Issue #8: a crash during a run's last attempt leaves the run running and
// its River job running. After the restart River's rescuer hands the job
// out again; the worker closes the open attempt, fails the run, wipes its
// data and reports it once, without calling the system again. The crash
// is simulated in SQL, with the River job's limit as the worker leaves it
// during a last attempt (checked above).
func TestCrashDuringTheLastAttemptFailsAndReportsTheRunAfterRestart(t *testing.T) {
	rep := &fakeReporter{}
	h := newHarness(t, nil, nil, func(d *Deps) {
		d.Reporter = rep
		d.RetryDelays = []time.Duration{100 * time.Millisecond}
	},
		jobSpec{"citadel", schedule.WakeNone, "send-reminders", 10, 0})
	ctx := context.Background()
	// The old process: stopped before it could pick anything up.
	if err := db.StopRiver(h.sched.River, time.Second); err != nil {
		t.Fatal(err)
	}
	var id string
	err := pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
		var jobID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM job`).Scan(&jobID); err != nil {
			return err
		}
		var err error
		id, err = Enqueue(ctx, tx, h.sched.River, jobID, "queue", []byte(`{"course_id":42}`), time.Now(), 10, 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE run SET state = 'running', attempts = 1, started_at = now() - interval '2 hours' WHERE id = $1`,
		`INSERT INTO attempt (run_id, number, started_at) VALUES ($1, 1, now() - interval '2 hours')`,
		`UPDATE river_job SET state = 'running', attempt = 1, max_attempts = 2, attempted_at = now() - interval '2 hours',
			attempted_by = ARRAY['crashed-process'], scheduled_at = now() - interval '2 hours'
		WHERE args->>'run_id' = $1`,
	} {
		if _, err := h.pool.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}

	h.restart()
	r := h.wait(id, 20*time.Second)
	if r.state != "failed" || r.attempts != 1 || r.hasData || !r.finished || r.lastError != "interrupted: PARADOX stopped during the attempt" {
		t.Fatalf("after restart: run %+v", r)
	}
	if got := h.attemptLog(id); strings.Join(got, ",") != "1 error: interrupted: PARADOX stopped during the attempt" {
		t.Fatalf("attempts %v", got)
	}
	if n := h.hits.Load(); n != 0 {
		t.Fatalf("system called %d times, want none", n)
	}
	fs := rep.list()
	if len(fs) != 1 || fs[0].RunID != id || fs[0].Attempts != 1 || fs[0].LastError != "interrupted: PARADOX stopped during the attempt" {
		t.Fatalf("reports %+v, want one", fs)
	}
	for end := time.Now().Add(5 * time.Second); ; {
		state, _, _ := h.riverJob(id)
		if state == "cancelled" {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("River job %s, want cancelled", state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// River rescues a stuck job once both db.RescueStuckJobsAfter and the
// worker's own Timeout (wake timeout, plus the job's limit, plus a margin)
// have passed, so the floor is short and River's one-hour default is not
// used. River must also accept the setting.
func TestStuckJobsAreRescuedAfterTheFloorAndTheJobsOwnLimit(t *testing.T) {
	cfg := db.RiverConfig(river.NewWorkers(), 1)
	if cfg.RescueStuckJobsAfter != db.RescueStuckJobsAfter || db.RescueStuckJobsAfter <= 0 || db.RescueStuckJobsAfter >= time.Hour {
		t.Fatalf("RescueStuckJobsAfter %v, want the documented floor %v, under an hour", cfg.RescueStuckJobsAfter, db.RescueStuckJobsAfter)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &Worker{D: &Deps{}})
	cfg = db.RiverConfig(workers, 1)
	// The pool connects lazily, so no database is needed to check the config.
	pool, err := pgxpool.New(context.Background(), "postgres://paradox@localhost:1/paradox")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := river.NewClient(riverpgxv5.New(pool), cfg); err != nil {
		t.Fatalf("River refuses the config: %v", err)
	}
	w := &Worker{D: &Deps{WakeTimeout: 2 * time.Minute}}
	job := &river.Job[RunArgs]{Args: RunArgs{TimeoutS: 600}}
	if got, want := w.Timeout(job), 2*time.Minute+10*time.Minute+30*time.Second; got != want {
		t.Fatalf("worker Timeout %v, want %v (wake timeout + job limit + margin)", got, want)
	}
}
