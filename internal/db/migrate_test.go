package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// freshDB opens the test database and drops PARADOX's tables, so each test
// starts from an empty schema. River's tables are left alone.
func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := Open(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `DROP TABLE IF EXISTS run_action, queue_request, attempt, run, job, system, paradox_migration CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestMigrateCreatesAnnexATablesOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := freshDB(t)
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	for _, table := range []string{"system", "job", "queue_request", "run", "attempt", "run_action"} {
		var ok bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&ok); err != nil || !ok {
			t.Errorf("table %s missing (%v)", table, err)
		}
	}
	var n int
	pool.QueryRow(ctx, "SELECT count(*) FROM paradox_migration").Scan(&n)
	if n != 1 {
		t.Fatalf("%d migrations recorded, want 1", n)
	}
}

func TestSchemaEnforcesThePRDLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := freshDB(t)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustExec(`INSERT INTO system (name, wake_mode) VALUES ('citadel', 'sablier')`)
	mustExec(`INSERT INTO job (system_id, name, timeout_s, retries) SELECT id, 'send-reminders', 600, 3 FROM system`)
	var runID string
	if err := pool.QueryRow(ctx, `INSERT INTO run (job_id, source, due_at, state, max_attempts)
		SELECT id, 'schedule', now(), 'queued', 4 FROM job RETURNING id`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	mustExec(`INSERT INTO attempt (run_id, number, started_at) VALUES ($1, 1, now())`, runID)
	mustExec(`INSERT INTO run_action (run_id, action) VALUES ($1, 'cancel')`, runID)
	mustExec(`INSERT INTO queue_request (system_id, request_id, run_id, key_fingerprint) SELECT id, 'course-42.publish', $1, 'SHA256:x' FROM system`, runID)

	refused := []struct{ name, sql string }{
		{"unknown wake mode", `INSERT INTO system (name, wake_mode) VALUES ('x', 'always')`},
		{"duplicate system", `INSERT INTO system (name, wake_mode) VALUES ('citadel', 'none')`},
		{"duplicate job per system", `INSERT INTO job (system_id, name, timeout_s, retries) SELECT id, 'send-reminders', 60, 0 FROM system`},
		{"retries over 100", `INSERT INTO job (system_id, name, timeout_s, retries) SELECT id, 'j2', 60, 101 FROM system`},
		{"unknown run state", `UPDATE run SET state = 'paused'`},
		{"attempts over max", `UPDATE run SET attempts = 5`},
		{"error over 2 KB", `UPDATE run SET last_error = repeat('x', 2049)`},
		{"bad request ID", `UPDATE queue_request SET request_id = 'has space'`},
		{"unknown action", `INSERT INTO run_action (run_id, action) SELECT id, 'delete' FROM run`},
	}
	for _, c := range refused {
		if _, err := pool.Exec(ctx, c.sql); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}

	// Deleting a run (retention, PX-REC-02) takes its attempts, actions and
	// queue request with it.
	mustExec(`DELETE FROM run WHERE id = $1`, runID)
	var left int
	pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM attempt) + (SELECT count(*) FROM run_action) + (SELECT count(*) FROM queue_request)`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d dependent rows left after deleting the run", left)
	}
}

func TestMigrationNamesAreChecked(t *testing.T) {
	list, err := migrationList()
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range list {
		if m.version != i+1 || !strings.HasSuffix(m.name, ".sql") {
			t.Fatalf("migration %d is %+v; numbers must run 1, 2, 3 with no gaps", i, m)
		}
	}
}
