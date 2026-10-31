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
	_, err = rc.InsertTx(ctx, tx, RunArgs{RunID: runID, TimeoutS: timeoutS}, &river.InsertOpts{
		MaxAttempts: maxAttempts,
		ScheduledAt: dueAt,
	})
	return runID, err
}

// Tick creates one run for every repeating job due at or before now, and
// moves each job's next due time past now. It returns how many runs it
// created. Rows are locked with SKIP LOCKED, so two PARADOX processes
// during a redeploy overlap never create the same run twice.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) (int, error) {
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
			if _, err := Enqueue(ctx, tx, s.River, d.id, "schedule", nil, d.at, d.timeoutS, d.retries+1); err != nil {
				return err
			}
			// Next due after now, not after the old due time, so a job that
			// fell behind gets one run rather than one per missed slot.
			// Catching up and recording missed runs is PX-SCH-05's work.
			next := timetable.Next(sched, s.D.Location, now)
			var nextPtr *time.Time
			if !next.IsZero() {
				nextPtr = &next
			}
			if _, err := tx.Exec(ctx, `UPDATE job SET last_due_at = $2, next_due_at = $3 WHERE id = $1`, d.id, d.at, nextPtr); err != nil {
				return err
			}
			created++
		}
		return nil
	})
	return created, err
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
