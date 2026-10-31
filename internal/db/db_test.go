package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// testURL returns the local test database (dev/compose.yaml), or skips.
func testURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PARADOX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PARADOX_TEST_DATABASE_URL not set; start dev/compose.yaml to run database tests")
	}
	return url
}

type pingArgs struct{ N int }

func (pingArgs) Kind() string { return "paradox_test_ping" }

type pingWorker struct {
	river.WorkerDefaults[pingArgs]
	done chan int
}

func (w *pingWorker) Work(ctx context.Context, job *river.Job[pingArgs]) error {
	w.done <- job.Args.N
	return nil
}

// Through PgBouncer in session mode with a pool of 2, River must migrate,
// accept a job and run it within the 5 seconds of PX-NFR-02, while leaving
// PARADOX's own queries able to run.
func TestRiverRunsThroughSessionPoolOfTwo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := Open(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := MigrateRiver(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := MigrateRiver(ctx, pool); err != nil {
		t.Fatalf("second migrate must be a no-op: %v", err)
	}

	workers := river.NewWorkers()
	w := &pingWorker{done: make(chan int, 1)}
	river.AddWorker(workers, w)
	client, err := river.NewClient(riverpgxv5.New(pool), RiverConfig(workers, 10))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Stop(context.Background())

	inserted := time.Now()
	if _, err := client.Insert(ctx, pingArgs{N: 7}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-w.done:
		if n != 7 {
			t.Fatalf("worked job %d, want 7", n)
		}
		if took := time.Since(inserted); took > 5*time.Second {
			t.Fatalf("job started after %v, want within 5s (PX-NFR-02)", took)
		}
	case <-ctx.Done():
		t.Fatal("job never ran: River could not get a connection or never polled")
	}

	// PARADOX's own queries still get a connection while River runs.
	var one int
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("own query while River runs: %v", err)
	}
	if got := pool.Config().MaxConns; got != MaxConns {
		t.Fatalf("pool allows %d connections, want %d", got, MaxConns)
	}
}

func TestOpenHidesPasswordAndFailsFast(t *testing.T) {
	_, err := Open(context.Background(), "postgres://u:s3cret@%zz/db")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("got %v", err)
	}
}
