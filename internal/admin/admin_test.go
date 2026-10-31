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

	"github.com/Rumeasiyan/paradox/internal/db"
)

func setup(t *testing.T) (*httptest.Server, time.Time) {
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
	pool.Exec(ctx, `DROP TABLE IF EXISTS run_action, queue_request, attempt, run, job, system, paradox_migration CASCADE`)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	_, err = pool.Exec(ctx, `
		INSERT INTO system (name, wake_mode) VALUES ('citadel', 'sablier'), ('website', 'none');
		INSERT INTO job (system_id, name, timeout_s, retries) SELECT id, 'send-reminders', 600, 3 FROM system WHERE name = 'citadel';
		INSERT INTO job (system_id, name, timeout_s, retries) SELECT id, 'rebuild', 600, 3 FROM system WHERE name = 'website';
		INSERT INTO run (job_id, source, data, due_at, state, max_attempts, attempts, finished_at, last_error)
		SELECT j.id, v.source, v.data::jsonb, v.due::timestamptz, v.state, 4, v.attempts, v.fin::timestamptz, v.err
		FROM job j JOIN (VALUES
			('send-reminders', 'schedule', NULL, '2026-11-02 07:00Z', 'succeeded', 1, '2026-11-02 07:00:05Z', NULL),
			('send-reminders', 'queue', '{"learner":"secret-name"}', '2026-11-02 09:00Z', 'failed', 4, '2026-11-02 09:20Z', 'answered 500: boom'),
			('rebuild', 'queue', NULL, '2026-11-02 10:00Z', 'queued', 0, NULL, NULL),
			('send-reminders', 'schedule', NULL, '2026-10-30 07:00Z', 'succeeded', 1, '2026-10-30 07:00:05Z', NULL)
		) AS v(job, source, data, due, state, attempts, fin, err) ON v.job = j.name`)
	if err != nil {
		t.Fatal(err)
	}
	a := &Admin{Pool: pool, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv, now
}

func get(t *testing.T, srv *httptest.Server, query string) (int, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/admin/runs" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestListFiltersNewestFirst(t *testing.T) {
	srv, _ := setup(t)
	code, body := get(t, srv, "")
	var all []Run
	json.Unmarshal([]byte(body), &all)
	// Default window is 24 hours: the run from 30 October is left out.
	if code != 200 || len(all) != 3 || all[0].Job != "rebuild" || all[2].State != "succeeded" {
		t.Fatalf("%d %s", code, body)
	}
	for q, want := range map[string]int{"?state=failed": 1, "?system=website": 1, "?job=send-reminders": 2, "?since=168h": 4, "?limit=1": 1, "?system=nobody": 0} {
		code, body := get(t, srv, q)
		var got []Run
		json.Unmarshal([]byte(body), &got)
		if code != 200 || len(got) != want {
			t.Errorf("%s: %d runs (%d), want %d", q, len(got), code, want)
		}
	}
	_, body = get(t, srv, "?state=failed")
	if !strings.Contains(body, "answered 500: boom") || !strings.Contains(body, `"attempts":4`) {
		t.Fatalf("failed run lacks its error: %s", body)
	}
}

func TestListNeverShowsJobData(t *testing.T) {
	srv, _ := setup(t)
	_, body := get(t, srv, "?since=168h")
	if strings.Contains(body, "secret-name") || strings.Contains(body, `"data"`) {
		t.Fatalf("job data in the listing: %s", body)
	}
}

func TestListRefusesBadParameters(t *testing.T) {
	srv, _ := setup(t)
	for _, q := range []string{"?state=paused", "?since=-1h", "?since=soon", "?limit=0", "?limit=5000"} {
		if code, _ := get(t, srv, q); code != 400 {
			t.Errorf("%s: %d, want 400", q, code)
		}
	}
}
