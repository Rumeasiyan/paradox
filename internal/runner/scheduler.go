package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/robfig/cron/v3"

	"github.com/Rumeasiyan/paradox/internal/timetable"
)

// Scheduler turns due timetable entries into runs (how it works, Flow A
// step 4).
type Scheduler struct {
	D     *Deps
	River *river.Client[pgx.Tx]
	// CatchUp is PARADOX_CATCH_UP: how late a due time may be and still
	// run (PX-SCH-05). Zero means DefaultCatchUp.
	CatchUp time.Duration
}

// DefaultCatchUp is PARADOX_CATCH_UP when Scheduler.CatchUp is zero.
const DefaultCatchUp = time.Hour

func (s *Scheduler) catchUp() time.Duration {
	if s.CatchUp > 0 {
		return s.CatchUp
	}
	return DefaultCatchUp
}

// Enqueue creates a run and its River job in tx, so both exist or neither.
// The queue API (story 18) uses it too.
func Enqueue(ctx context.Context, tx pgx.Tx, rc *river.Client[pgx.Tx], jobID int64, source string, data []byte, dueAt time.Time, timeoutS, maxAttempts int) (string, error) {
	var runID string
	err := tx.QueryRow(ctx, `
		INSERT INTO run (job_id, source, data, due_at, state, max_attempts)
		VALUES ($1, $2, $3, $4, 'queued', $5) RETURNING id`,
		jobID, source, data, dueAt, maxAttempts).Scan(&runID)
	if err != nil {
		return "", err
	}
	// River inserts any job given a scheduled time as "scheduled" and makes
	// it available only on its 5-second maintenance cycle, which alone would
	// break the 5-second start of PX-NFR-02. So a job due now or earlier gets
	// no scheduled time (available at once), and a future one is scheduled
	// riverSchedulerLead early; the worker waits out the rest (waitUntilDue).
	opts := &river.InsertOpts{MaxAttempts: maxAttempts}
	if early := dueAt.Add(-riverSchedulerLead); early.After(time.Now()) {
		opts.ScheduledAt = early
	}
	_, err = rc.InsertTx(ctx, tx, RunArgs{RunID: runID, TimeoutS: timeoutS}, opts)
	return runID, err
}

// riverSchedulerLead matches River's job scheduler interval (5 seconds,
// not configurable): how early a future run's River job is scheduled so it
// is available by its due time.
const riverSchedulerLead = 5 * time.Second

// CancelRiverJobs cancels the River jobs carrying a run, in tx, so a
// cancelled run never wakes its system. Used by the queue API and the
// operator endpoint.
func CancelRiverJobs(ctx context.Context, tx pgx.Tx, rc *river.Client[pgx.Tx], runID string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM river_job WHERE args->>'run_id' = $1`, runID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := rc.JobCancelTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// Tick handles every repeating job due at or before now and moves each
// job's next due time past now. Of all a job's due times up to now:
//
//   - The most recent one older than now minus CatchUp, if any, is
//     recorded as one missed run (PX-SCH-05); older ones are not recorded.
//   - The most recent one within CatchUp, if any, runs once. If the job
//     still has a run from the schedule queued or running, it is recorded
//     as skipped instead (PX-SCH-06). Runs queued through the queue API
//     do not count.
//
// Missed and skipped runs are finished at once, with no attempt and no
// River job. Tick returns how many runs it queued. Rows are locked with
// SKIP LOCKED, so two PARADOX processes during a redeploy overlap never
// create the same run twice. All of it happens in one transaction with
// no slow work inside.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Add(-s.catchUp())
	created := 0
	err := pgx.BeginFunc(ctx, s.D.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT j.id, j.repeat, j.next_due_at, j.timeout_s, j.retries
			FROM job j JOIN system s ON s.id = j.system_id
			WHERE s.removed_at IS NULL AND j.repeat IS NOT NULL AND j.next_due_at <= $1
			ORDER BY j.next_due_at
			FOR UPDATE OF j SKIP LOCKED`, now)
		if err != nil {
			return err
		}
		type due struct {
			id                int64
			repeat            string
			at                time.Time
			timeoutS, retries int
		}
		var list []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.id, &d.repeat, &d.at, &d.timeoutS, &d.retries); err != nil {
				rows.Close()
				return err
			}
			list = append(list, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range list {
			sched, err := cron.ParseStandard(d.repeat)
			if err != nil {
				return fmt.Errorf("job %d: stored repeat %q no longer parses: %w", d.id, d.repeat, err)
			}
			loc := s.D.Location
			maxAttempts := d.retries + 1
			lastDue := d.at

			// PX-SCH-05: too late to catch up, so one missed run.
			first := d.at
			if first.Before(cutoff) {
				missed := lastSlot(sched, loc, first, func(t time.Time) bool { return t.Before(cutoff) })
				if err := finishedRun(ctx, tx, d.id, missed, "missed", maxAttempts, now); err != nil {
					return err
				}
				lastDue = missed
				first = timetable.Next(sched, loc, missed)
			}

			// The most recent due time within CatchUp runs once.
			if !first.IsZero() && !first.After(now) {
				at := lastSlot(sched, loc, first, func(t time.Time) bool { return !t.After(now) })
				var busy bool
				if err := tx.QueryRow(ctx, `
					SELECT EXISTS (SELECT 1 FROM run
					WHERE job_id = $1 AND source = 'schedule' AND state IN ('queued', 'running'))`,
					d.id).Scan(&busy); err != nil {
					return err
				}
				if busy {
					// PX-SCH-06: a scheduled job never runs twice at once.
					if err := finishedRun(ctx, tx, d.id, at, "skipped", maxAttempts, now); err != nil {
						return err
					}
				} else {
					if _, err := Enqueue(ctx, tx, s.River, d.id, "schedule", nil, at, d.timeoutS, maxAttempts); err != nil {
						return err
					}
					created++
				}
				lastDue = at
			}

			// Next due after now, not after the last due time, so a job that
			// fell behind never gets one run per slot it was down for.
			next := timetable.Next(sched, loc, now)
			var nextPtr *time.Time
			if !next.IsZero() {
				nextPtr = &next
			}
			if _, err := tx.Exec(ctx, `UPDATE job SET last_due_at = $2, next_due_at = $3 WHERE id = $1`, d.id, lastDue, nextPtr); err != nil {
				return err
			}
		}
		return nil
	})
	return created, err
}

// finishedRun records a run from the schedule that ends as it is created
// (missed or skipped): no attempt and no River job.
func finishedRun(ctx context.Context, tx pgx.Tx, jobID int64, dueAt time.Time, state string, maxAttempts int, now time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO run (job_id, source, due_at, state, max_attempts, finished_at)
		VALUES ($1, 'schedule', $2, $3, $4, $5)`,
		jobID, dueAt, state, maxAttempts, now)
	return err
}

// lastSlot returns the latest due time of sched, from first on, for which
// ok holds. first must be a due time with ok(first) true, and ok must be
// true for every due time up to some point and false for all after it.
//
// Walking one due time at a time could take long for a job due every
// minute after a long outage, so it first jumps ahead in doubling steps
// while a passing due time lies beyond the step, then halves the step,
// and walks only the last minute.
func lastSlot(sched cron.Schedule, loc *time.Location, first time.Time, ok func(time.Time) bool) time.Time {
	lo := first
	step := time.Minute
	grow := true
	for step >= time.Minute {
		probe := timetable.Next(sched, loc, lo.Add(step))
		if !probe.IsZero() && ok(probe) {
			lo = probe
			if grow {
				step *= 2
			}
			continue
		}
		// No passing due time after lo+step: any later one lies in
		// (lo, lo+step].
		grow = false
		step /= 2
	}
	for {
		n := timetable.Next(sched, loc, lo)
		if n.IsZero() || !ok(n) {
			return lo
		}
		lo = n
	}
}

// Run calls Tick every interval until ctx ends. Errors are logged; the
// next tick tries again.
func (s *Scheduler) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := s.Tick(ctx, s.D.now()); err != nil {
			if ctx.Err() == nil {
				s.D.Logger.Error("scheduler tick failed", "error", err)
			}
		} else if n > 0 {
			s.D.Logger.Debug("runs created", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
