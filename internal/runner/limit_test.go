package runner

import (
	"context"
	"testing"
	"time"

	"github.com/Rumeasiyan/paradox/internal/schedule"
)

// PX-RUN-04: no more than PARADOX_MAX_RUNNING runs at the same time across
// all systems. River's worker count is set from it, so with a limit of 2
// and five slow jobs due together, at most two are ever running.
func TestRunsAtTheSameTimeStayWithinTheLimit(t *testing.T) {
	harnessMaxRunning = 2
	defer func() { harnessMaxRunning = 10 }()
	var jobs []jobSpec
	slow := map[string]time.Duration{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		jobs = append(jobs, jobSpec{"citadel", schedule.WakeNone, name, 10, 0})
		slow[name] = 600 * time.Millisecond
	}
	h := newHarness(t, nil, slow, nil, jobs...)
	ctx := context.Background()
	h.pool.Exec(ctx, `UPDATE job SET next_due_at = now() - interval '1 second'`)
	if n, err := h.sched.Tick(ctx, time.Now()); err != nil || n != 5 {
		t.Fatalf("tick created %d runs, err %v", n, err)
	}
	most := 0
	end := time.Now().Add(20 * time.Second)
	for time.Now().Before(end) {
		var running, done int
		h.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'running'), count(*) FILTER (WHERE state = 'succeeded') FROM run`).Scan(&running, &done)
		if running > most {
			most = running
		}
		if done == 5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if most > 2 {
		t.Fatalf("%d runs at once, limit 2", most)
	}
	if most < 2 {
		t.Fatalf("never ran 2 at once (max %d); the test did not exercise the limit", most)
	}
	var done int
	h.pool.QueryRow(ctx, `SELECT count(*) FROM run WHERE state = 'succeeded'`).Scan(&done)
	if done != 5 {
		t.Fatalf("%d of 5 runs succeeded", done)
	}
}
