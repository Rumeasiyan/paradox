package timetable

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rumeasiyan/paradox/internal/systems"
)

// Sync writes the systems read at start into the timetable, in one
// transaction, so a failure leaves the previous timetable as it was
// (PX-SYS-05: changes take effect on restart).
//
//   - Each system is added or updated, and is no longer marked removed.
//   - A system missing from list is marked removed at now. Its row and its
//     jobs stay, so old runs still resolve, but its jobs are never due.
//   - Each job is added or updated. A queue-only job has no next due time.
//     A repeating job keeps its stored next due time if its every text is
//     unchanged, so runs missed while PARADOX was down can be caught up;
//     otherwise it gets Next(schedule, loc, now).
//   - A job missing from its system's file keeps its row, but is never due.
func Sync(ctx context.Context, pool *pgxpool.Pool, list []systems.System, loc *time.Location, now time.Time) error {
	if len(list) == 0 {
		// systems.Load never returns an empty list; accepting one would mark
		// every system removed.
		return errors.New("timetable: no systems to load")
	}
	now = now.UTC()
	names := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, s := range list {
		if seen[s.Name] {
			return fmt.Errorf("timetable: system %s appears twice", s.Name)
		}
		seen[s.Name] = true
		names = append(names, s.Name)
	}

	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, s := range list {
			if err := syncSystem(ctx, tx, s, loc, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE system SET removed_at = $1
			WHERE removed_at IS NULL AND NOT (name = ANY($2::text[]))`, now, names); err != nil {
			return fmt.Errorf("timetable: marking removed systems: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE job SET next_due_at = NULL
			WHERE next_due_at IS NOT NULL
			  AND system_id IN (SELECT id FROM system WHERE NOT (name = ANY($1::text[])))`, names); err != nil {
			return fmt.Errorf("timetable: stopping jobs of removed systems: %w", err)
		}
		return nil
	})
}

func syncSystem(ctx context.Context, tx pgx.Tx, s systems.System, loc *time.Location, now time.Time) error {
	var address *string
	if s.Address != "" {
		address = &s.Address
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO system (name, wake_mode, address) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE
		SET wake_mode = EXCLUDED.wake_mode, address = EXCLUDED.address, removed_at = NULL
		RETURNING id`, s.Name, s.Wake, address).Scan(&id)
	if err != nil {
		return fmt.Errorf("timetable: system %s: %w", s.Name, err)
	}

	jobNames := make([]string, 0, len(s.Jobs))
	for _, j := range s.Jobs {
		jobNames = append(jobNames, j.Name)
		var repeat *string
		var due *time.Time
		if j.Every != "" {
			if j.Repeat == nil {
				return fmt.Errorf("timetable: job %s/%s: every %q was not parsed", s.Name, j.Name, j.Every)
			}
			repeat = &j.Every
			if n := Next(j.Repeat, loc, now); !n.IsZero() {
				due = &n
			}
		}
		if j.Timeout <= 0 {
			return fmt.Errorf("timetable: job %s/%s: timeout must be positive", s.Name, j.Name)
		}
		// Whole seconds, rounded up so a short timeout never becomes zero.
		timeout := int64(math.Ceil(j.Timeout.Seconds()))

		// The stored next due time is kept only when the every text is the
		// same as before and a time is stored; story 25 catches up from it.
		_, err := tx.Exec(ctx, `INSERT INTO job (system_id, name, repeat, timeout_s, retries, next_due_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (system_id, name) DO UPDATE
			SET repeat = EXCLUDED.repeat,
			    timeout_s = EXCLUDED.timeout_s,
			    retries = EXCLUDED.retries,
			    next_due_at = CASE
			        WHEN EXCLUDED.repeat IS NULL THEN NULL
			        WHEN job.repeat IS NOT DISTINCT FROM EXCLUDED.repeat AND job.next_due_at IS NOT NULL THEN job.next_due_at
			        ELSE EXCLUDED.next_due_at
			    END`, id, j.Name, repeat, timeout, j.Retries, due)
		if err != nil {
			return fmt.Errorf("timetable: job %s/%s: %w", s.Name, j.Name, err)
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE job SET next_due_at = NULL
		WHERE system_id = $1 AND next_due_at IS NOT NULL AND NOT (name = ANY($2::text[]))`, id, jobNames); err != nil {
		return fmt.Errorf("timetable: system %s: stopping removed jobs: %w", s.Name, err)
	}
	return nil
}
