package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/robfig/cron/v3"

	"github.com/Rumeasiyan/paradox/internal/db"
	"github.com/Rumeasiyan/paradox/internal/schedule"
	"github.com/Rumeasiyan/paradox/internal/systems"
	"github.com/Rumeasiyan/paradox/internal/timetable"
	"github.com/Rumeasiyan/paradox/sign"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// env is a running API with two systems, citadel and website, each with
// its own key, on the local test database. River is not started, so
// queued runs stay queued.
type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	srv    *httptest.Server
	keys   map[string]ed25519.PrivateKey
	logs   *lockedBuffer
	now    time.Time
	api    *API
	client *http.Client
}

func newEnv(t *testing.T) *env {
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
	if err := db.MigrateRiver(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS run_action, queue_request, attempt, run, job, system, paradox_migration CASCADE; DELETE FROM river_job`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	keys := map[string]ed25519.PrivateKey{}
	var list []systems.System
	daily, _ := cron.ParseStandard("0 7 * * *")
	for _, spec := range []struct{ name, job string }{{"citadel", "generate-certificates"}, {"website", "rebuild"}} {
		pubText, privText, _ := sign.GenerateKey()
		pub, _ := sign.ParsePublicKey(pubText)
		keys[spec.name], _ = sign.ParsePrivateKey(privText)
		list = append(list, systems.System{
			System: schedule.System{Name: spec.name, Wake: schedule.WakeNone, Jobs: []schedule.Job{
				{Name: spec.job, Timeout: 10 * time.Minute, Retries: 3},
				{Name: "daily", Every: "0 7 * * *", Repeat: daily, Timeout: time.Minute, Retries: 0},
			}},
			Keys: []ed25519.PublicKey{pub},
		})
	}
	if err := timetable.Sync(ctx, pool, list, time.UTC, time.Now()); err != nil {
		t.Fatal(err)
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: pool, keys: keys, logs: &lockedBuffer{}, now: time.Now().UTC().Truncate(time.Second), client: &http.Client{}}
	e.api = &API{
		Pool:          pool,
		River:         rc,
		Keys:          systems.NewIndex(list).Keys,
		Window:        5 * time.Minute,
		MaxDataBytes:  1024,
		MaxAhead:      30 * 24 * time.Hour,
		RequestIDKeep: 24 * time.Hour,
		Logger:        slog.New(slog.NewJSONHandler(e.logs, nil)),
		Now:           func() time.Time { return e.now },
	}
	e.srv = httptest.NewServer(e.api.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

// call sends a request signed as system (empty system: unsigned).
func (e *env) call(system, method, path string, body any, mutate func(*http.Request)) (int, map[string]any) {
	e.t.Helper()
	var raw []byte
	switch b := body.(type) {
	case nil:
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(raw))
	if system != "" {
		s := sign.Signer{KeyID: system, Key: e.keys[system], Direction: sign.ToParadox, Now: func() time.Time { return e.now }}
		if err := s.Sign(req, raw); err != nil {
			e.t.Fatal(err)
		}
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func queue(job, requestID string, extra map[string]any) map[string]any {
	m := map[string]any{"job": job, "request_id": requestID}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestQueueOwnJob(t *testing.T) {
	e := newEnv(t)
	code, out := e.call("citadel", "POST", Prefix, queue("generate-certificates", "course-42.publish", map[string]any{"data": map[string]any{"course_id": 42}}), nil)
	if code != http.StatusAccepted || out["state"] != "queued" || out["repeat"] != false || out["job"] != "generate-certificates" {
		t.Fatalf("%d %v", code, out)
	}
	runID, _ := out["run_id"].(string)
	var source, state, fp string
	var data []byte
	var due time.Time
	err := e.pool.QueryRow(context.Background(), `
		SELECT r.source, r.state, r.data::text, r.due_at, q.key_fingerprint
		FROM run r JOIN queue_request q ON q.run_id = r.id WHERE r.id = $1`, runID).Scan(&source, &state, &data, &due, &fp)
	if err != nil {
		t.Fatal(err)
	}
	if source != "queue" || state != "queued" || !strings.Contains(string(data), "42") || !due.Equal(e.now) || !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("stored %s %s %s %v %s", source, state, data, due, fp)
	}
	var riverJobs int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM river_job WHERE args->>'run_id' = $1`, runID).Scan(&riverJobs)
	if riverJobs != 1 {
		t.Fatalf("%d River jobs for the run", riverJobs)
	}
}

func TestRunAtIsConvertedToUTCAndLimited(t *testing.T) {
	e := newEnv(t)
	at := e.now.Add(6 * time.Hour).In(time.FixedZone("+05:30", 5*3600+1800)).Format(time.RFC3339)
	code, out := e.call("citadel", "POST", Prefix, queue("generate-certificates", "later", map[string]any{"run_at": at}), nil)
	if code != http.StatusAccepted || out["due_at"] != e.now.Add(6*time.Hour).Format(time.RFC3339) {
		t.Fatalf("future: %d %v", code, out)
	}
	code, out = e.call("citadel", "POST", Prefix, queue("generate-certificates", "past", map[string]any{"run_at": e.now.Add(-time.Hour).Format(time.RFC3339)}), nil)
	if code != http.StatusAccepted || out["due_at"] != e.now.Format(time.RFC3339) {
		t.Fatalf("past runs now: %d %v", code, out)
	}
	code, _ = e.call("citadel", "POST", Prefix, queue("generate-certificates", "far", map[string]any{"run_at": e.now.Add(31 * 24 * time.Hour).Format(time.RFC3339)}), nil)
	if code != http.StatusBadRequest {
		t.Fatalf("31 days ahead: %d (PX-JOB-02)", code)
	}
	code, _ = e.call("citadel", "POST", Prefix, queue("generate-certificates", "nozone", map[string]any{"run_at": "2026-11-01T22:00:00"}), nil)
	if code != http.StatusBadRequest {
		t.Fatalf("no offset: %d", code)
	}
}

func TestBadRequestsAreRefused(t *testing.T) {
	e := newEnv(t)
	for name, c := range map[string]struct {
		body any
		want int
	}{
		"not json":         {"{nope", http.StatusBadRequest},
		"unknown field":    {map[string]any{"job": "generate-certificates", "request_id": "x", "owner": "me"}, http.StatusBadRequest},
		"missing job":      {map[string]any{"request_id": "x"}, http.StatusBadRequest},
		"missing request":  {map[string]any{"job": "generate-certificates"}, http.StatusBadRequest},
		"bad request ID":   {queue("generate-certificates", "has space", nil), http.StatusBadRequest},
		"data over limit":  {queue("generate-certificates", "big", map[string]any{"data": strings.Repeat("x", 1100)}), http.StatusRequestEntityTooLarge},
		"body over limit":  {strings.Repeat(" ", 1024+8<<10+1), http.StatusRequestEntityTooLarge},
		"two JSON values":  {`{"job":"generate-certificates","request_id":"a"} {}`, http.StatusBadRequest},
		"other system job": {queue("rebuild", "x", nil), http.StatusForbidden},
		"unknown job":      {queue("delete-everything", "x", nil), http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			code, out := e.call("citadel", "POST", Prefix, c.body, nil)
			if code != c.want {
				t.Fatalf("%d %v, want %d", code, out, c.want)
			}
		})
	}
	var runs int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("%d runs queued by refused requests", runs)
	}
}

// PX-SIG-02 to PX-SIG-05: nothing is queued without a valid signature from
// a current system, made for this direction, within the window.
func TestSignatureIsRequired(t *testing.T) {
	e := newEnv(t)
	body := queue("generate-certificates", "x", map[string]any{"data": "course-secret-42"})
	cases := map[string]struct {
		system string
		mutate func(*http.Request)
		clock  time.Duration
	}{
		"unsigned": {system: ""},
		"claims another system": {system: "citadel", mutate: func(r *http.Request) {
			r.Header.Set("Signature-Input", strings.Replace(r.Header.Get("Signature-Input"), `keyid="citadel"`, `keyid="website"`, 1))
		}},
		"unknown system": {system: "citadel", mutate: func(r *http.Request) {
			r.Header.Set("Signature-Input", strings.Replace(r.Header.Get("Signature-Input"), `keyid="citadel"`, `keyid="stranger"`, 1))
		}},
		"body changed after signing": {system: "citadel", mutate: func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{"job":"generate-certificates","request_id":"y"}`))
			r.ContentLength = -1
		}},
		"too old": {system: "citadel", clock: -6 * time.Minute},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			saved := e.now
			if c.clock != 0 {
				// Sign with a clock in the past, check with the real one.
				e.now = saved.Add(c.clock)
				e.api.Now = func() time.Time { return saved }
				defer func() { e.now = saved; e.api.Now = func() time.Time { return e.now } }()
			}
			code, out := e.call(c.system, "POST", Prefix, body, c.mutate)
			if code != http.StatusUnauthorized || out["error"] != "not_authenticated" {
				t.Fatalf("%d %v", code, out)
			}
		})
	}
	t.Run("wrong direction", func(t *testing.T) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", e.srv.URL+Prefix, bytes.NewReader(raw))
		sign.Signer{KeyID: "citadel", Key: e.keys["citadel"], Direction: sign.ToSystem, Now: func() time.Time { return e.now }}.Sign(req, raw)
		resp, _ := e.client.Do(req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%d", resp.StatusCode)
		}
	})
	var runs int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("%d runs queued without a valid signature", runs)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, `"claimed_system":"stranger"`) {
		t.Fatalf("refusal log lacks the claimed system:\n%s", logs)
	}
	if strings.Contains(logs, "course-secret-42") {
		t.Fatal("job data reached the log")
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	e := newEnv(t)
	code, out := e.call("citadel", "GET", Prefix+"/a/b/c", nil, nil)
	if code != http.StatusNotFound || out["error"] != "not_found" {
		t.Fatalf("%d %v", code, out)
	}
}
