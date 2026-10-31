package runner

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Rumeasiyan/paradox/internal/schedule"
)

// schedRun is one run row as the scheduler rules see it.
type schedRun struct {
	source, state string
	due           time.Time
	attempts      int
	finished      *time.Time
}

func (h *harness) runsOf(job string) []schedRun {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(), `
		SELECT r.source, r.state, r.due_at, r.attempts, r.finished_at
		FROM run r JOIN job j ON j.id = r.job_id WHERE j.name = $1 ORDER BY r.due_at, r.state`, job)
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (schedRun, error) {
		var r schedRun
		err := row.Scan(&r.source, &r.state, &r.due, &r.attempts, &r.finished)
		return r, err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *harness) riverJobs() int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM river_job`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// setJob sets a job's repeat and next due time.
func (h *harness) setJob(job, repeat string, next time.Time) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), `UPDATE job SET repeat = $2, next_due_at = $3 WHERE name = $1`, job, repeat, next); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) tick(now time.Time, want int) {
	h.t.Helper()
	n, err := h.sched.Tick(context.Background(), now)
	if err != nil || n != want {
		h.t.Fatalf("tick queued %d runs, err %v; want %d", n, err, want)
	}
}

// nextAfter fails unless the job's next due time is after now.
func (h *harness) nextAfter(job string, now time.Time) time.Time {
	h.t.Helper()
	var next *time.Time
	if err := h.pool.QueryRow(context.Background(), `SELECT next_due_at FROM job WHERE name = $1`, job).Scan(&next); err != nil {
		h.t.Fatal(err)
	}
	if next == nil || !next.After(now) {
		h.t.Fatalf("next_due_at %v, want after %v", next, now)
	}
	return *next
}

func (h *harness) jobID(job string) int64 {
	h.t.Helper()
	var id int64
	if err := h.pool.QueryRow(context.Background(), `SELECT id FROM job WHERE name = $1`, job).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) waitState(job, state string, within time.Duration) {
	h.t.Helper()
	end := time.Now().Add(within)
	for {
		var n int
		h.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM run r JOIN job j ON j.id = r.job_id
			WHERE j.name = $1 AND r.source = 'schedule' AND r.state = $2`, job, state).Scan(&n)
		if n > 0 {
			return
		}
		if time.Now().After(end) {
			h.t.Fatalf("no run of %s reached %s within %v", job, state, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A date far ahead keeps these tests independent of the wall clock. Runs
// queued for it never start, which these tests do not need.
var later = time.Date(2030, 6, 15, 12, 0, 30, 0, time.UTC)

// PX-SCH-05: a due time less than PARADOX_CATCH_UP late runs once.
func TestDueTimeWithinCatchUpRuns(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeNone, "report", 10, 0})
	now := time.Now()
	h.setJob("report", "0 7 * * *", now.Add(-30*time.Minute))
	h.tick(now, 1)
	runs := h.runsOf("report")
	if len(runs) != 1 || runs[0].source != "schedule" {
		t.Fatalf("runs %+v", runs)
	}
	h.waitState("report", "succeeded", 10*time.Second)
	h.nextAfter("report", now)
	if n := h.riverJobs(); n != 1 {
		t.Fatalf("%d River jobs, want 1", n)
	}
}

// PX-SCH-05: beyond PARADOX_CATCH_UP the due time does not run; one missed
// run is recorded and no River job is made.
func TestDueTimeBeyondCatchUpIsMissed(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeNone, "report", 10, 2})
	due := time.Date(2030, 6, 15, 7, 0, 0, 0, time.UTC)
	h.setJob("report", "0 7 * * *", due)
	h.tick(later, 0)
	runs := h.runsOf("report")
	if len(runs) != 1 {
		t.Fatalf("runs %+v, want one", runs)
	}
	r := runs[0]
	if r.state != "missed" || r.source != "schedule" || !r.due.Equal(due) || r.attempts != 0 || r.finished == nil || !r.finished.Equal(later) {
		t.Fatalf("run %+v", r)
	}
	if n := h.riverJobs(); n != 0 {
		t.Fatalf("%d River jobs, want 0", n)
	}
	if next := h.nextAfter("report", later); !next.Equal(due.AddDate(0, 0, 1)) {
		t.Fatalf("next_due_at %v", next)
	}
	// A second tick at the same time finds nothing due.
	h.tick(later, 0)
	if runs := h.runsOf("report"); len(runs) != 1 {
		t.Fatalf("runs after second tick %+v", runs)
	}
}

// PX-SCH-05 with a wider PARADOX_CATCH_UP: the same late due time runs.
func TestCatchUpSettingWidensTheWindow(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeNone, "report", 10, 0})
	h.sched.CatchUp = 6 * time.Hour
	due := time.Date(2030, 6, 15, 7, 0, 0, 0, time.UTC)
	h.setJob("report", "0 7 * * *", due)
	h.tick(later, 1)
	runs := h.runsOf("report")
	if len(runs) != 1 || runs[0].state != "queued" || !runs[0].due.Equal(due) {
		t.Fatalf("runs %+v", runs)
	}
	h.nextAfter("report", later)
}

// PX-SCH-05: days of due times every five minutes give one missed run, for
// the latest one beyond the window, and one run for the latest within it.
func TestManyMissedDueTimesGiveOneMissedRun(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeNone, "sync", 10, 0})
	now := time.Date(2030, 6, 15, 12, 2, 30, 0, time.UTC)
	h.setJob("sync", "*/5 * * * *", time.Date(2030, 6, 12, 0, 0, 0, 0, time.UTC))
	h.tick(now, 1)
	runs := h.runsOf("sync")
	if len(runs) != 2 {
		t.Fatalf("runs %+v, want two", runs)
	}
	if r := runs[0]; r.state != "missed" || !r.due.Equal(time.Date(2030, 6, 15, 11, 0, 0, 0, time.UTC)) || r.finished == nil {
		t.Fatalf("missed run %+v", r)
	}
	if r := runs[1]; r.state != "queued" || !r.due.Equal(time.Date(2030, 6, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("queued run %+v", r)
	}
	if n := h.riverJobs(); n != 1 {
		t.Fatalf("%d River jobs, want 1", n)
	}
	if next := h.nextAfter("sync", now); !next.Equal(time.Date(2030, 6, 15, 12, 5, 0, 0, time.UTC)) {
		t.Fatalf("next_due_at %v", next)
	}
}

// PX-SCH-05: missed with nothing inside the window gives only the missed
// run, for the latest due time.
func TestMissedDueTimesWithNoneInTheWindow(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeNone, "sync", 10, 0})
	now := time.Date(2030, 6, 15, 12, 2, 30, 0, time.UTC)
	h.setJob("sync", "30 */3 * * *", time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC))
	h.tick(now, 0)
	runs := h.runsOf("sync")
	if len(runs) != 1 || runs[0].state != "missed" || !runs[0].due.Equal(time.Date(2030, 6, 15, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("runs %+v", runs)
	}
	if next := h.nextAfter("sync", now); !next.Equal(time.Date(2030, 6, 15, 12, 30, 0, 0, time.UTC)) {
		t.Fatalf("next_due_at %v", next)
	}
}

// PX-SCH-06: while the previous scheduled run is running, the next due
// time is recorded as skipped; once it has finished, the job runs again.
func TestOverlapIsSkippedThenRunsAgain(t *testing.T) {
	h := newHarness(t, nil, map[string]time.Duration{"slow": 1500 * time.Millisecond}, nil,
		jobSpec{"citadel", schedule.WakeNone, "slow", 10, 0})
	now := time.Now()
	h.setJob("slow", "* * * * *", now.Add(-time.Second))
	h.tick(now, 1)
	h.waitState("slow", "running", 10*time.Second)

	now = time.Now()
	h.setJob("slow", "* * * * *", now.Add(-time.Second))
	h.tick(now, 0)
	h.nextAfter("slow", now)
	var skipped []schedRun
	for _, r := range h.runsOf("slow") {
		if r.state == "skipped" {
			skipped = append(skipped, r)
		}
	}
	if len(skipped) != 1 || skipped[0].attempts != 0 || skipped[0].finished == nil {
		t.Fatalf("skipped runs %+v", skipped)
	}
	if n := h.riverJobs(); n != 1 {
		t.Fatalf("%d River jobs, want 1", n)
	}

	h.waitState("slow", "succeeded", 10*time.Second)
	now = time.Now()
	h.setJob("slow", "* * * * *", now.Add(-time.Second))
	h.tick(now, 1)
	h.nextAfter("slow", now)
	end := time.Now().Add(10 * time.Second)
	for {
		var done int
		h.pool.QueryRow(context.Background(), `SELECT count(*) FROM run WHERE state = 'succeeded'`).Scan(&done)
		if done == 2 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("second run did not succeed: %+v", h.runsOf("slow"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// PX-SCH-06: a scheduled run still queued blocks the next one too.
func TestQueuedScheduledRunCausesSkip(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeNone, "report", 10, 0})
	h.enqueue("report", "schedule", later.Add(time.Hour))
	h.setJob("report", "0 7 * * *", later.Add(-time.Minute))
	h.tick(later, 0)
	runs := h.runsOf("report")
	if len(runs) != 2 || runs[0].state != "skipped" || runs[1].state != "queued" {
		t.Fatalf("runs %+v", runs)
	}
	h.nextAfter("report", later)
}

// PX-SCH-06 is about scheduled runs only: a run of the same job queued
// through the queue API does not cause a skip.
func TestQueuedThroughTheAPIDoesNotCauseSkip(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeNone, "report", 10, 0})
	h.enqueue("report", "queue", later.Add(time.Hour))
	h.setJob("report", "0 7 * * *", later.Add(-time.Minute))
	h.tick(later, 1)
	runs := h.runsOf("report")
	if len(runs) != 2 || runs[0].source != "schedule" || runs[0].state != "queued" || runs[1].source != "queue" {
		t.Fatalf("runs %+v", runs)
	}
	h.nextAfter("report", later)
}

func (h *harness) enqueue(job, source string, at time.Time) {
	h.t.Helper()
	ctx := context.Background()
	id := h.jobID(job)
	err := pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
		_, err := Enqueue(ctx, tx, h.sched.River, id, source, nil, at, 10, 1)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
}
