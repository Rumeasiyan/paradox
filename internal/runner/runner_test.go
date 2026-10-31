package runner

import (
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/robfig/cron/v3"

	"github.com/Rumeasiyan/paradox/internal/db"
	"github.com/Rumeasiyan/paradox/internal/schedule"
	"github.com/Rumeasiyan/paradox/internal/systems"
	"github.com/Rumeasiyan/paradox/internal/testsystem"
	"github.com/Rumeasiyan/paradox/internal/timetable"
	"github.com/Rumeasiyan/paradox/internal/wake"
	"github.com/Rumeasiyan/paradox/sign"
)

// harness is a whole local PARADOX: database, River, the scheduler, the
// fake waker and the test system standing in for the systems.
type harness struct {
	t     *testing.T
	pool  *pgxpool.Pool
	sched *Scheduler
	waker *wake.Fake
	hits  *atomic.Int32
}

type jobSpec struct {
	system, wakeMode, job string
	timeoutS, retries     int
}

func newHarness(t *testing.T, fail []string, slow map[string]time.Duration, d func(*Deps), jobs ...jobSpec) *harness {
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
	if err := db.MigrateRiver(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS run_action, queue_request, attempt, run, job, system, paradox_migration CASCADE; DELETE FROM river_job`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	pubText, privText, _ := sign.GenerateKey()
	pub, _ := sign.ParsePublicKey(pubText)
	priv, _ := sign.ParsePrivateKey(privText)

	failSet := map[string]bool{}
	for _, f := range fail {
		failSet[f] = true
	}
	ts := httptest.NewServer(testsystem.New(testsystem.Config{ParadoxKeys: []ed25519.PublicKey{pub}, FailJobs: failSet, SlowJobs: slow}, nil))
	t.Cleanup(ts.Close)
	hits := new(atomic.Int32)
	counted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		proxy, _ := http.NewRequestWithContext(r.Context(), r.Method, ts.URL+r.URL.Path, r.Body)
		proxy.Header = r.Header
		resp, err := http.DefaultClient.Do(proxy)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(counted.Close)

	var order []string
	bySystem := map[string]*systems.System{}
	for _, j := range jobs {
		s, ok := bySystem[j.system]
		if !ok {
			s = &systems.System{System: schedule.System{Name: j.system, Wake: j.wakeMode}}
			bySystem[j.system] = s
			order = append(order, j.system)
		}
		sched, _ := cron.ParseStandard("0 7 * * *")
		s.Jobs = append(s.Jobs, schedule.Job{Name: j.job, Every: "0 7 * * *", Repeat: sched, Timeout: time.Duration(j.timeoutS) * time.Second, Retries: j.retries})
	}
	var list []systems.System
	for _, name := range order {
		list = append(list, *bySystem[name])
	}
	if err := timetable.Sync(ctx, pool, list, time.UTC, time.Now()); err != nil {
		t.Fatal(err)
	}

	waker := wake.NewFake()
	deps := &Deps{
		Pool:           pool,
		Waker:          waker,
		Key:            sign.Signer{KeyID: sign.ParadoxKeyID, Key: priv, Direction: sign.ToSystem},
		HTTP:           &http.Client{},
		Logger:         slog.New(slog.DiscardHandler),
		Location:       time.UTC,
		WakeTimeout:    2 * time.Second,
		KeepAwakeEvery: 100 * time.Millisecond,
		BaseURL:        func(string, string) string { return counted.URL },
	}
	if d != nil {
		d(deps)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &Worker{D: deps})
	cfg := db.RiverConfig(workers, 10)
	cfg.Logger = slog.New(slog.DiscardHandler)
	rc, err := river.NewClient(riverpgxv5.New(pool), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rc.Stop(stop)
	})
	return &harness{t: t, pool: pool, sched: &Scheduler{D: deps, River: rc}, waker: waker, hits: hits}
}

// due makes a job due now and ticks the scheduler once.
func (h *harness) due(job string) string {
	h.t.Helper()
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `UPDATE job SET next_due_at = now() - interval '1 second' WHERE name = $1`, job); err != nil {
		h.t.Fatal(err)
	}
	n, err := h.sched.Tick(ctx, time.Now())
	if err != nil || n != 1 {
		h.t.Fatalf("tick created %d runs, err %v; want 1", n, err)
	}
	var id string
	if err := h.pool.QueryRow(ctx, `SELECT r.id FROM run r JOIN job j ON j.id = r.job_id WHERE j.name = $1`, job).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

type runRow struct {
	state, lastError string
	attempts         int
	hasData          bool
	finished         bool
}

// wait polls until the run reaches a final state.
func (h *harness) wait(runID string, within time.Duration) runRow {
	h.t.Helper()
	end := time.Now().Add(within)
	for {
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
		if r.state == "succeeded" || r.state == "failed" {
			return r
		}
		if time.Now().After(end) {
			h.t.Fatalf("run still %s after %v", r.state, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *harness) attempts(runID string) []string {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(), `SELECT outcome || ':' || coalesce(status_code::text, '-') FROM attempt WHERE run_id = $1 ORDER BY number`, runID)
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func TestScheduledJobWakesRunsAndIsRecorded(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeSablier, "send-reminders", 10, 3})
	id := h.due("send-reminders")
	r := h.wait(id, 10*time.Second)
	if r.state != "succeeded" || r.attempts != 1 || !r.finished || r.hasData {
		t.Fatalf("run %+v", r)
	}
	if got := h.attempts(id); len(got) != 1 || got[0] != "success:200" {
		t.Fatalf("attempts %v", got)
	}
	if calls := h.waker.WakeCalls(); len(calls) != 1 || calls[0].System != "citadel" {
		t.Fatalf("wake calls %v", calls)
	}
}

func TestAlwaysOnSystemIsNotWoken(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"site", schedule.WakeNone, "publish", 10, 0})
	r := h.wait(h.due("publish"), 10*time.Second)
	if r.state != "succeeded" || len(h.waker.WakeCalls()) != 0 {
		t.Fatalf("run %+v, wake calls %d", r, len(h.waker.WakeCalls()))
	}
}

func TestFailingJobFailsAndRecordsTheAnswer(t *testing.T) {
	h := newHarness(t, []string{"fail"}, nil, nil, jobSpec{"citadel", schedule.WakeSablier, "fail", 10, 0})
	id := h.due("fail")
	r := h.wait(id, 10*time.Second)
	if r.state != "failed" || r.attempts != 1 || !strings.Contains(r.lastError, "answered 500") {
		t.Fatalf("run %+v", r)
	}
	if got := h.attempts(id); len(got) != 1 || got[0] != "error:500" {
		t.Fatalf("attempts %v", got)
	}
}

func TestWakeTimeoutCountsAsFailedAttemptWithoutCalling(t *testing.T) {
	h := newHarness(t, nil, nil, func(d *Deps) { d.WakeTimeout = 200 * time.Millisecond }, jobSpec{"citadel", schedule.WakeSablier, "send-reminders", 10, 0})
	h.waker.NeverReady("citadel")
	id := h.due("send-reminders")
	r := h.wait(id, 10*time.Second)
	if r.state != "failed" || !strings.Contains(r.lastError, "not ready within") {
		t.Fatalf("run %+v", r)
	}
	if got := h.attempts(id); len(got) != 1 || got[0] != "wake_timeout:-" {
		t.Fatalf("attempts %v", got)
	}
	if h.hits.Load() != 0 {
		t.Fatalf("system was called %d times while asleep", h.hits.Load())
	}
}

func TestSlowAnswerPastTheTimeLimitIsATimeout(t *testing.T) {
	h := newHarness(t, nil, map[string]time.Duration{"slow": 3 * time.Second}, nil, jobSpec{"citadel", schedule.WakeNone, "slow", 1, 0})
	id := h.due("slow")
	r := h.wait(id, 10*time.Second)
	if r.state != "failed" || !strings.Contains(r.lastError, "no answer within 1s") {
		t.Fatalf("run %+v", r)
	}
	if got := h.attempts(id); len(got) != 1 || got[0] != "timeout:-" {
		t.Fatalf("attempts %v", got)
	}
}

func TestSystemIsKeptAwakeWhileTheJobRuns(t *testing.T) {
	h := newHarness(t, nil, map[string]time.Duration{"long": 450 * time.Millisecond}, nil, jobSpec{"citadel", schedule.WakeSablier, "long", 5, 0})
	r := h.wait(h.due("long"), 10*time.Second)
	if r.state != "succeeded" {
		t.Fatalf("run %+v", r)
	}
	if n := len(h.waker.KeepAwakeCalls()); n < 2 {
		t.Fatalf("kept awake %d times during a 450 ms job at 100 ms, want at least 2", n)
	}
}

func TestCancelledRunIsNeverCalled(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeNone, "send-reminders", 10, 0})
	ctx := context.Background()
	h.pool.Exec(ctx, `UPDATE job SET next_due_at = now() + interval '1 hour'`)
	var id string
	err := pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
		var jobID int64
		tx.QueryRow(ctx, `SELECT id FROM job`).Scan(&jobID)
		var err error
		id, err = Enqueue(ctx, tx, h.sched.River, jobID, "queue", []byte(`{"x":1}`), time.Now().Add(time.Second), 10, 1)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE run SET state = 'cancelled' WHERE id = $1`, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if h.hits.Load() != 0 || len(h.attempts(id)) != 0 {
		t.Fatalf("cancelled run was called: hits %d", h.hits.Load())
	}
}

func TestTickCreatesOneRunAndMovesPastNow(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeNone, "send-reminders", 10, 0})
	ctx := context.Background()
	// Due three days ago: still only one run, and the next due time is in the future.
	h.pool.Exec(ctx, `UPDATE job SET next_due_at = now() - interval '3 days'`)
	now := time.Now()
	if n, err := h.sched.Tick(ctx, now); err != nil || n != 1 {
		t.Fatalf("first tick: %d, %v", n, err)
	}
	if n, err := h.sched.Tick(ctx, now); err != nil || n != 0 {
		t.Fatalf("second tick: %d, %v", n, err)
	}
	var next, last time.Time
	h.pool.QueryRow(ctx, `SELECT next_due_at, last_due_at FROM job`).Scan(&next, &last)
	if !next.After(now) || !last.Before(now.Add(-48*time.Hour)) {
		t.Fatalf("next %v last %v", next, last)
	}
}
