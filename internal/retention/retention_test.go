package retention

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rumeasiyan/paradox/internal/db"
)

const (
	recordKeep    = 90 * 24 * time.Hour
	requestIDKeep = 24 * time.Hour
	failedKeep    = 7 * 24 * time.Hour
)

// freshDB drops and recreates PARADOX's tables and returns a pool with one
// system and one job (id 1 each).
func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("PARADOX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PARADOX_TEST_DATABASE_URL not set; start dev/compose.yaml to run database tests")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS run_action, queue_request, attempt, run, job, system, paradox_migration, qr_away CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO system (name, wake_mode) VALUES ('citadel', 'none')`)
	exec(t, pool, `INSERT INTO job (system_id, name, timeout_s, retries) VALUES (1, 'nightly', 60, 0)`)
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// addRun inserts a run with the given state, finished_at (nil for none)
// and data (empty for NULL), and returns its id.
func addRun(t *testing.T, pool *pgxpool.Pool, state string, finished *time.Time, data string) string {
	t.Helper()
	var d any
	if data != "" {
		d = data
	}
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO run (job_id, source, data, due_at, state, max_attempts, finished_at)
		VALUES (1, 'queue', $1::jsonb, $2, $3, 1, $4) RETURNING id::text`,
		d, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), state, finished).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// addChildren gives a run one attempt, one run action and a queue request
// received at received.
func addChildren(t *testing.T, pool *pgxpool.Pool, runID, requestID string, received time.Time) {
	t.Helper()
	exec(t, pool, `INSERT INTO attempt (run_id, number, started_at) VALUES ($1, 1, $2)`, runID, received)
	exec(t, pool, `INSERT INTO run_action (run_id, by_system_id, action) VALUES ($1, 1, 'retry')`, runID)
	exec(t, pool, `INSERT INTO queue_request (system_id, request_id, run_id, received_at, key_fingerprint) VALUES (1, $1, $2, $3, 'fp')`, requestID, runID, received)
}

func exists(t *testing.T, pool *pgxpool.Pool, runID string) bool {
	return count(t, pool, `SELECT count(*) FROM run WHERE id = $1`, runID) == 1
}

func at(t time.Time) *time.Time { return &t }

func TestSweepDeletesOldRunsWithTheirRowsAndKeepsTheRest(t *testing.T) {
	pool := freshDB(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	old := now.Add(-recordKeep - time.Hour)
	recent := now.Add(-recordKeep + time.Hour)

	var oldRuns []string
	for _, state := range []string{"succeeded", "failed", "cancelled", "skipped", "missed"} {
		id := addRun(t, pool, state, at(old), "")
		addChildren(t, pool, id, "old-"+state, old)
		oldRuns = append(oldRuns, id)
	}
	recentRun := addRun(t, pool, "succeeded", at(recent), "")
	addChildren(t, pool, recentRun, "recent", now.Add(-time.Hour))
	// Open runs older than the keep stay, however old (finished_at is set
	// here on purpose, to show the state alone protects them).
	queued := addRun(t, pool, "queued", nil, `{"n": 1}`)
	running := addRun(t, pool, "running", at(old), `{"n": 2}`)
	exec(t, pool, `UPDATE run SET due_at = $1`, old.Add(-365*24*time.Hour))

	res, err := Sweep(context.Background(), pool, now, recordKeep, requestIDKeep, failedKeep)
	if err != nil {
		t.Fatal(err)
	}
	if res.Runs != 5 {
		t.Errorf("Runs = %d, want 5", res.Runs)
	}
	for _, id := range oldRuns {
		if exists(t, pool, id) {
			t.Errorf("old run %s still there", id)
		}
	}
	for _, table := range []string{"attempt", "run_action", "queue_request"} {
		if n := count(t, pool, `SELECT count(*) FROM `+table+` WHERE run_id <> $1`, recentRun); n != 0 {
			t.Errorf("%d %s rows of old runs left", n, table)
		}
		if n := count(t, pool, `SELECT count(*) FROM `+table+` WHERE run_id = $1`, recentRun); n != 1 {
			t.Errorf("recent run has %d %s rows, want 1", n, table)
		}
	}
	for name, id := range map[string]string{"recent": recentRun, "queued": queued, "running": running} {
		if !exists(t, pool, id) {
			t.Errorf("%s run deleted", name)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM run WHERE id IN ($1, $2) AND data IS NOT NULL`, queued, running); n != 2 {
		t.Errorf("open runs lost their data: %d of 2 still hold it", n)
	}
}

func TestSweepDeletesOldRequestIDsButKeepsTheirRuns(t *testing.T) {
	pool := freshDB(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	oldReq := addRun(t, pool, "succeeded", at(finished), "")
	addChildren(t, pool, oldReq, "old", now.Add(-requestIDKeep-time.Minute))
	newReq := addRun(t, pool, "queued", nil, `{}`)
	addChildren(t, pool, newReq, "new", now.Add(-requestIDKeep+time.Minute))

	res, err := Sweep(context.Background(), pool, now, recordKeep, requestIDKeep, failedKeep)
	if err != nil {
		t.Fatal(err)
	}
	if res.QueueRequests != 1 || res.Runs != 0 {
		t.Errorf("Result = %+v, want 1 queue request and 0 runs", res)
	}
	if n := count(t, pool, `SELECT count(*) FROM queue_request WHERE request_id = 'old'`); n != 0 {
		t.Error("old request ID still there")
	}
	if n := count(t, pool, `SELECT count(*) FROM queue_request WHERE request_id = 'new'`); n != 1 {
		t.Error("recent request ID deleted")
	}
	if !exists(t, pool, oldReq) || !exists(t, pool, newReq) {
		t.Error("a run went with its request ID")
	}
	if n := count(t, pool, `SELECT count(*) FROM attempt`); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
}

func TestSweepWipesDataLeftOnFinishedRuns(t *testing.T) {
	pool := freshDB(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var final []string
	for _, state := range []string{"succeeded", "cancelled", "skipped", "missed"} {
		final = append(final, addRun(t, pool, state, at(now.Add(-time.Minute)), `{"email": "someone@example.com"}`))
	}
	// A failed run keeps its data for the failed-data keep so the operator
	// can retry it (PX-REC-04), then loses it like any other.
	recentFail := addRun(t, pool, "failed", at(now.Add(-6*24*time.Hour)), `{"email": "someone@example.com"}`)
	oldFail := addRun(t, pool, "failed", at(now.Add(-8*24*time.Hour)), `{"email": "someone@example.com"}`)
	final = append(final, oldFail)
	queued := addRun(t, pool, "queued", nil, `{"keep": true}`)
	running := addRun(t, pool, "running", nil, `{"keep": true}`)

	res, err := Sweep(context.Background(), pool, now, recordKeep, requestIDKeep, failedKeep)
	if err != nil {
		t.Fatal(err)
	}
	if res.DataWiped != 5 || res.Runs != 0 {
		t.Errorf("Result = %+v, want 5 wiped and 0 runs deleted", res)
	}
	for _, id := range final {
		if n := count(t, pool, `SELECT count(*) FROM run WHERE id = $1 AND data IS NULL`, id); n != 1 {
			t.Errorf("finished run %s still holds data", id)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM run WHERE id = $1 AND data IS NOT NULL`, recentFail); n != 1 {
		t.Error("a failed run lost its data inside the keep")
	}
	if n := count(t, pool, `SELECT count(*) FROM run WHERE id IN ($1, $2) AND data IS NOT NULL`, queued, running); n != 2 {
		t.Error("data of an open run was wiped")
	}
	// A second sweep finds nothing left to do.
	res, err = Sweep(context.Background(), pool, now, recordKeep, requestIDKeep, failedKeep)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed() {
		t.Errorf("second sweep removed %+v", res)
	}
}

func TestSweepWorksInBatches(t *testing.T) {
	pool := freshDB(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	old := now.Add(-recordKeep - time.Hour)
	total := 2*BatchSize + 500
	// Old finished runs, each with an attempt and a queue request.
	exec(t, pool, `
		WITH r AS (
			INSERT INTO run (job_id, source, due_at, state, max_attempts, finished_at)
			SELECT 1, 'queue', $1, 'succeeded', 1, $1 FROM generate_series(1, $2)
			RETURNING id)
		INSERT INTO queue_request (system_id, request_id, run_id, received_at, key_fingerprint)
		SELECT 1, 'r-' || id::text, id, $1, 'fp' FROM r`, old, total)
	exec(t, pool, `INSERT INTO attempt (run_id, number, started_at) SELECT id, 1, $1 FROM run`, old)
	// Recent finished runs with old request IDs and leftover data.
	exec(t, pool, `
		WITH r AS (
			INSERT INTO run (job_id, source, data, due_at, state, max_attempts, finished_at)
			SELECT 1, 'queue', '{"x": 1}', $1, 'succeeded', 1, $1 FROM generate_series(1, $2)
			RETURNING id)
		INSERT INTO queue_request (system_id, request_id, run_id, received_at, key_fingerprint)
		SELECT 1, 'n-' || id::text, id, $3, 'fp' FROM r`, now.Add(-time.Hour), total, now.Add(-requestIDKeep-time.Hour))

	res, err := Sweep(context.Background(), pool, now, recordKeep, requestIDKeep, failedKeep)
	if err != nil {
		t.Fatal(err)
	}
	want := Result{Runs: int64(total), QueueRequests: int64(total), DataWiped: int64(total)}
	if res != want {
		t.Errorf("Result = %+v, want %+v", res, want)
	}
	if n := count(t, pool, `SELECT count(*) FROM run`); n != total {
		t.Errorf("runs left = %d, want %d", n, total)
	}
	if n := count(t, pool, `SELECT count(*) FROM attempt`) + count(t, pool, `SELECT count(*) FROM queue_request`); n != 0 {
		t.Errorf("%d attempt or queue_request rows left, want 0", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM run WHERE data IS NOT NULL`); n != 0 {
		t.Errorf("%d runs still hold data", n)
	}
}

// syncBuffer is a bytes.Buffer safe for the loop goroutine and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startLoop runs Loop and returns a stop function that cancels it and
// checks it returns.
func startLoop(t *testing.T, pool *pgxpool.Pool, every time.Duration, logger *slog.Logger) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Loop(ctx, pool, every, recordKeep, requestIDKeep, failedKeep, logger)
		close(done)
	}()
	return func() {
		t.Helper()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Loop did not return after cancel")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLoopSweepsAtOnceAndStopsOnCancel(t *testing.T) {
	pool := freshDB(t)
	old := addRun(t, pool, "succeeded", at(time.Now().Add(-recordKeep-time.Hour)), `{"secret": "x"}`)
	var logs syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	stop := startLoop(t, pool, time.Hour, logger)
	waitFor(t, "the first sweep", func() bool { return !exists(t, pool, old) })
	waitFor(t, "the Info log", func() bool { return strings.Contains(logs.String(), `"runs":1`) })
	stop()

	out := logs.String()
	if !strings.Contains(out, `"level":"INFO"`) {
		t.Errorf("counts not logged at Info: %s", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("job data reached the log: %s", out)
	}
}

func TestLoopLogsErrorsAndCarriesOn(t *testing.T) {
	pool := freshDB(t)
	run := addRun(t, pool, "succeeded", at(time.Now().Add(-time.Hour)), "")
	addChildren(t, pool, run, "old", time.Now().Add(-requestIDKeep-time.Hour))
	// Hide queue_request so the first sweep fails part way.
	exec(t, pool, `ALTER TABLE queue_request RENAME TO qr_away`)
	var logs syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	stop := startLoop(t, pool, 50*time.Millisecond, logger)
	defer stop()
	waitFor(t, "the error log", func() bool { return strings.Contains(logs.String(), "retention sweep failed") })
	exec(t, pool, `ALTER TABLE qr_away RENAME TO queue_request`)
	waitFor(t, "a later sweep", func() bool {
		return count(t, pool, `SELECT count(*) FROM queue_request`) == 0
	})
	if !exists(t, pool, run) {
		t.Error("run deleted with its request ID")
	}
}

func TestLoopReturnsAtOnceWhenAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		// A nil pool would panic if Loop tried to sweep.
		Loop(ctx, nil, time.Hour, recordKeep, requestIDKeep, failedKeep, slog.New(slog.DiscardHandler))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Loop did not return for a cancelled context")
	}
}
