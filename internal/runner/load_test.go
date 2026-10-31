package runner

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Rumeasiyan/paradox/internal/schedule"
)

// TestCapacity checks PX-NFR-01 and PX-NFR-02 on this machine: 50 systems,
// a burst of 2000 runs (about five hours of the PRD's 10,000 a day), then a
// steady 2 runs a second (17 times the PRD) where every run must start
// within 5 seconds of its due time, all within the memory limit. A last
// phase at 20 runs a second is logged, not judged. The test's own inserts
// share the two-connection pool, so it is harder than production. It is
// slow, so it runs only with PARADOX_LOAD_TEST=1.
func TestCapacity(t *testing.T) {
	if os.Getenv("PARADOX_LOAD_TEST") != "1" {
		t.Skip("set PARADOX_LOAD_TEST=1 to run the capacity test")
	}
	var jobs []jobSpec
	for i := 0; i < 50; i++ {
		jobs = append(jobs, jobSpec{fmt.Sprintf("system-%02d", i), schedule.WakeNone, fmt.Sprintf("job-%02d", i), 10, 0})
	}
	h := newHarness(t, nil, nil, nil, jobs...)
	ctx := context.Background()

	var peak atomic.Uint64
	stopWatch := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.Sys > peak.Load() {
				peak.Store(m.Sys)
			}
			select {
			case <-stopWatch:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	defer close(stopWatch)

	var jobIDs []int64
	rows, _ := h.pool.Query(ctx, `SELECT id FROM job ORDER BY id`)
	jobIDs, _ = pgx.CollectRows(rows, pgx.RowTo[int64])
	enqueue := func(n int, due time.Time) {
		for done := 0; done < n; {
			err := pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
				for i := 0; i < 100 && done < n; i++ {
					if _, err := Enqueue(ctx, tx, h.sched.River, jobIDs[done%len(jobIDs)], "queue", []byte(`{"n":1}`), due, 10, 1); err != nil {
						return err
					}
					done++
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	finished := func() int {
		var n int
		h.pool.QueryRow(ctx, `SELECT count(*) FROM run WHERE state = 'succeeded'`).Scan(&n)
		return n
	}

	// Phase 1: a burst of 2000 runs due at once.
	start := time.Now()
	enqueue(2000, start)
	for finished() < 2000 {
		if time.Since(start) > 5*time.Minute {
			t.Fatalf("burst: only %d of 2000 done after 5 minutes", finished())
		}
		time.Sleep(200 * time.Millisecond)
	}
	burst := time.Since(start)
	t.Logf("burst: 2000 runs in %v (%.0f runs/s, %.0f a day)", burst.Round(time.Millisecond), 2000/burst.Seconds(), 2000/burst.Seconds()*86400)

	// steady queues one run every gap for span, each due as it is queued,
	// waits for all of them to finish, and returns how many there were and
	// the average and worst delay between due time and start.
	steady := func(gap, span time.Duration) (int, float64, float64) {
		from := time.Now()
		want := finished()
		for time.Since(from) < span {
			enqueue(1, time.Now())
			want++
			time.Sleep(gap)
		}
		for finished() < want {
			if time.Since(from) > span+2*time.Minute {
				t.Fatalf("steady load: %d of %d runs finished", finished(), want)
			}
			time.Sleep(100 * time.Millisecond)
		}
		var n int
		var worst, avg float64
		if err := h.pool.QueryRow(ctx, `
			SELECT count(*), max(extract(epoch FROM started_at - due_at)), avg(extract(epoch FROM started_at - due_at))
			FROM run WHERE due_at >= $1`, from).Scan(&n, &worst, &avg); err != nil {
			t.Fatal(err)
		}
		return n, avg, worst
	}

	// Phase 2, judged: 2 runs a second (about 170,000 a day, 17 times the
	// PRD's 10,000), each must start within 5 seconds (PX-NFR-02).
	poolStats := func(label string) {
		st := h.pool.Stat()
		waits := st.EmptyAcquireCount()
		var avgWait time.Duration
		if waits > 0 {
			avgWait = st.EmptyAcquireWaitTime() / time.Duration(waits)
		}
		t.Logf("pool %s: acquires %d, had to wait %d (avg wait %v), max conns %d", label, st.AcquireCount(), waits, avgWait.Round(time.Millisecond), st.MaxConns())
	}
	poolStats("after burst")
	n, avg, worst := steady(500*time.Millisecond, 30*time.Second)
	poolStats("after 2 runs/s")
	t.Logf("steady at 2 runs/s: %d runs, start delay avg %.2fs, worst %.2fs", n, avg, worst)
	if worst > 5 {
		t.Errorf("a run started %.2fs after its due time, limit 5s (PX-NFR-02)", worst)
	}

	// Phase 3, informational: 20 runs a second (170 times the PRD), to show
	// where the two-connection pool starts to delay starts. Not judged.
	n, avg, worst = steady(50*time.Millisecond, 20*time.Second)
	t.Logf("steady at 20 runs/s (informational): %d runs, start delay avg %.2fs, worst %.2fs", n, avg, worst)

	mib := float64(peak.Load()) / (1 << 20)
	t.Logf("peak memory obtained from the OS: %.0f MiB (test process, including the test system and harness)", mib)
	if mib > 200 {
		t.Errorf("peak memory %.0f MiB, above the 200 MiB GOMEMLIMIT target (PX-NFR-01)", mib)
	}
}
