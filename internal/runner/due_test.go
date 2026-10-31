package runner

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Rumeasiyan/paradox/internal/schedule"
)

// A run due now goes to River as available at once; a future one is
// scheduled riverSchedulerLead early; and a run whose job is picked up
// early never starts before its due time.
func TestRiverJobsAreAvailableInTime(t *testing.T) {
	h := newHarness(t, nil, nil, nil, jobSpec{"citadel", schedule.WakeNone, "ping", 10, 0})
	ctx := context.Background()
	var jobID int64
	h.pool.QueryRow(ctx, `SELECT id FROM job`).Scan(&jobID)
	h.pool.Exec(ctx, `UPDATE job SET next_due_at = now() + interval '1 day'`)

	enqueue := func(due time.Time) string {
		var id string
		err := pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
			var err error
			id, err = Enqueue(ctx, tx, h.sched.River, jobID, "queue", nil, due, 10, 1)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	riverState := func(runID string) (string, time.Time) {
		var state string
		var at time.Time
		h.pool.QueryRow(ctx, `SELECT state, scheduled_at FROM river_job WHERE args->>'run_id' = $1`, runID).Scan(&state, &at)
		return state, at
	}

	far := time.Now().Add(time.Hour).Truncate(time.Second)
	farID := enqueue(far)
	if state, at := riverState(farID); state != "scheduled" || !at.Equal(far.Add(-riverSchedulerLead)) {
		t.Fatalf("future run: River job %s at %v, want scheduled at %v", state, at, far.Add(-riverSchedulerLead))
	}

	nowID := enqueue(time.Now())
	r := h.wait(nowID, 5*time.Second)
	var delay float64
	h.pool.QueryRow(ctx, `SELECT extract(epoch FROM started_at - due_at) FROM run WHERE id = $1`, nowID).Scan(&delay)
	if r.state != "succeeded" || delay > 2 {
		t.Fatalf("run due now: %+v, started %.2fs after due", r, delay)
	}

	soon := time.Now().Add(3 * time.Second)
	soonID := enqueue(soon)
	h.wait(soonID, 15*time.Second)
	var started time.Time
	h.pool.QueryRow(ctx, `SELECT started_at FROM run WHERE id = $1`, soonID).Scan(&started)
	if started.Before(soon) || started.After(soon.Add(2*time.Second)) {
		t.Fatalf("run due at %v started at %v", soon, started)
	}
}
