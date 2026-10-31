package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/Rumeasiyan/paradox/internal/db"
)

type actionsEnv struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	now  time.Time
	ids  map[string]string
}

// Five runs of citadel's send-reminders (retries 3): scheduled failed,
// queued failed 2 days ago with data, queued failed 8 days ago (data
// wiped), queued (with a River job) and succeeded.
func newActionsEnv(t *testing.T) *actionsEnv {
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
	db.MigrateRiver(ctx, pool)
	pool.Exec(ctx, `DROP TABLE IF EXISTS run_action, queue_request, attempt, run, job, system, paradox_migration CASCADE; DELETE FROM river_job`)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	pool.Exec(ctx, `
		INSERT INTO system (name, wake_mode) VALUES ('citadel', 'sablier');
		INSERT INTO job (system_id, name, timeout_s, retries) SELECT id, 'send-reminders', 600, 3 FROM system`)
	ids := map[string]string{}
	for _, r := range []struct{ key, source, state, data, finished string }{
		{"sched-failed", "schedule", "failed", "", "2026-11-02 07:30Z"},
		{"queue-failed-recent", "queue", "failed", `{"course":42}`, "2026-10-31 12:00Z"},
		{"queue-failed-old", "queue", "failed", "", "2026-10-25 12:00Z"},
		{"queued", "queue", "queued", `{"course":43}`, ""},
		{"done", "schedule", "succeeded", "", "2026-11-02 07:00Z"},
	} {
		var id string
		err := pool.QueryRow(ctx, `
			INSERT INTO run (job_id, source, data, due_at, state, max_attempts, attempts, finished_at, reported_at)
			SELECT id, $1, nullif($2, '')::jsonb, '2026-11-02 07:00Z', $3, 4,
			       CASE WHEN $3 = 'failed' THEN 4 ELSE 0 END, nullif($4, '')::timestamptz,
			       CASE WHEN $3 = 'failed' THEN now() END
			FROM job RETURNING id`, r.source, r.data, r.state, r.finished).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids[r.key] = id
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// The queued run has its River job, as the queue API would have made.
	if _, err := rc.Insert(ctx, testArgs{RunID: ids["queued"]}, nil); err != nil {
		t.Fatal(err)
	}
	a := &Admin{Pool: pool, River: rc, FailedDataKeep: 7 * 24 * time.Hour, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &actionsEnv{pool: pool, srv: srv, now: now, ids: ids}
}

// testArgs stands in for the runner's job args; only args.run_id matters.
type testArgs struct {
	RunID string `json:"run_id"`
}

func (testArgs) Kind() string { return "paradox_run" }

func (e *actionsEnv) post(t *testing.T, key, action string) (int, map[string]any) {
	t.Helper()
	id := e.ids[key]
	if id == "" {
		id = key
	}
	resp, err := http.Post(e.srv.URL+"/admin/runs/"+id+"/"+action, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func (e *actionsEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRetryGivesAFailedRunFreshAttempts(t *testing.T) {
	e := newActionsEnv(t)
	for _, key := range []string{"sched-failed", "queue-failed-recent"} {
		code, out := e.post(t, key, "retry")
		if code != 200 || out["state"] != "queued" {
			t.Fatalf("%s: %d %v", key, code, out)
		}
		id := e.ids[key]
		// 4 attempts made, job retries 3: room for 4 more.
		if n := e.count(t, `SELECT count(*) FROM run WHERE id = $1 AND max_attempts = 8 AND finished_at IS NULL AND reported_at IS NULL`, id); n != 1 {
			t.Errorf("%s: run not reset for new attempts", key)
		}
		if n := e.count(t, `SELECT count(*) FROM river_job WHERE args->>'run_id' = $1 AND state = 'available'`, id); n != 1 {
			t.Errorf("%s: %d River jobs ready", key, n)
		}
		if n := e.count(t, `SELECT count(*) FROM run_action WHERE run_id = $1 AND action = 'retry' AND by_system_id IS NULL`, id); n != 1 {
			t.Errorf("%s: retry not recorded as the operator's", key)
		}
	}
	// The queued run kept its data for the retry.
	if n := e.count(t, `SELECT count(*) FROM run WHERE id = $1 AND data->>'course' = '42'`, e.ids["queue-failed-recent"]); n != 1 {
		t.Error("retried queued run lost its data")
	}
}

func TestRetryRefusals(t *testing.T) {
	e := newActionsEnv(t)
	for key, want := range map[string]string{
		"queue-failed-old": "ask the system to queue it again",
		"queued":           "only failed runs can be retried",
		"done":             "only failed runs can be retried",
	} {
		code, out := e.post(t, key, "retry")
		if code != 409 || !strings.Contains(out["error"].(string), want) {
			t.Errorf("%s: %d %v", key, code, out)
		}
	}
	if code, _ := e.post(t, "00000000-0000-0000-0000-000000000000", "retry"); code != 404 {
		t.Errorf("unknown run: %d", code)
	}
	if code, _ := e.post(t, "not-a-uuid", "retry"); code != 404 {
		t.Errorf("malformed run ID: %d", code)
	}
	if n := e.count(t, `SELECT count(*) FROM run_action`); n != 0 {
		t.Errorf("%d actions recorded for refused requests", n)
	}
}

func TestCancelByTheOperator(t *testing.T) {
	e := newActionsEnv(t)
	code, out := e.post(t, "queued", "cancel")
	if code != 200 || out["state"] != "cancelled" {
		t.Fatalf("%d %v", code, out)
	}
	id := e.ids["queued"]
	if n := e.count(t, `SELECT count(*) FROM run WHERE id = $1 AND data IS NULL AND finished_at IS NOT NULL`, id); n != 1 {
		t.Error("cancelled run kept its data or has no finish time")
	}
	if n := e.count(t, `SELECT count(*) FROM river_job WHERE args->>'run_id' = $1 AND state = 'cancelled'`, id); n != 1 {
		t.Error("River job not cancelled")
	}
	if n := e.count(t, `SELECT count(*) FROM run_action WHERE run_id = $1 AND action = 'cancel' AND by_system_id IS NULL`, id); n != 1 {
		t.Error("cancel not recorded as the operator's")
	}
	if code, _ := e.post(t, "queued", "cancel"); code != 409 {
		t.Errorf("second cancel: %d", code)
	}
	if code, _ := e.post(t, "sched-failed", "cancel"); code != 409 {
		t.Errorf("cancel of a failed run: %d", code)
	}
}
