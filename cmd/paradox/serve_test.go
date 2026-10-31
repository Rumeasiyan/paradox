package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rumeasiyan/paradox/internal/db"
	"github.com/Rumeasiyan/paradox/internal/health"
	"github.com/Rumeasiyan/paradox/internal/testsystem"
	"github.com/Rumeasiyan/paradox/sign"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// The whole service on this machine: settings, systems folder, migrations,
// timetable, River, scheduler, health and a due job run end to end.
func TestServeRunsADueJobEndToEnd(t *testing.T) {
	dbURL := os.Getenv("PARADOX_TEST_DATABASE_URL")
	directURL := os.Getenv("PARADOX_TEST_DIRECT_URL")
	if dbURL == "" || directURL == "" {
		t.Skip("PARADOX_TEST_DATABASE_URL and PARADOX_TEST_DIRECT_URL not set; start dev/compose.yaml to run this test")
	}
	ctx := context.Background()
	// The test watches the database directly, not through PgBouncer: the
	// service under test takes both of PgBouncer's session connections.
	pool, err := db.Open(ctx, directURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	db.MigrateRiver(ctx, pool)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS run_action, queue_request, attempt, run, job, system, paradox_migration CASCADE; DELETE FROM river_job`); err != nil {
		t.Fatal(err)
	}

	pubText, privText, _ := sign.GenerateKey()
	pub, _ := sign.ParsePublicKey(pubText)
	sysPub, sysPriv, _ := sign.GenerateKey()

	// The test system plays the part of a system named "localhost", which
	// PARADOX calls at http://localhost:<PARADOX_SYSTEM_PORT>.
	ts := httptest.NewServer(testsystem.New(testsystem.Config{ParadoxKeys: []ed25519.PublicKey{pub}}, nil))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "localhost"), 0o755)
	os.WriteFile(filepath.Join(dir, "localhost", "schedules.yaml"), []byte("system: localhost\nwake: none\njobs:\n  - name: ping\n    every: \"0 7 * * *\"\n    retries: 0\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "localhost", "system.pub"), []byte(sysPub+"\n"), 0o644)

	freeLn, _ := net.Listen("tcp", "127.0.0.1:0")
	adminAddr := freeLn.Addr().String()
	freeLn.Close()
	env := map[string]string{
		"PARADOX_ADMIN_LISTEN": adminAddr,
		"PARADOX_SYSTEMS_DIR":  dir,
		"PARADOX_DATABASE_URL": dbURL,
		"PARADOX_PRIVATE_KEY":  privText,
		"PARADOX_LISTEN":       "127.0.0.1:0",
		"PARADOX_SYSTEM_PORT":  u.Port(),
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	addr := make(chan string, 1)
	var logs syncBuffer
	done := make(chan error, 1)
	go func() { done <- serve(sctx, &logs, envFrom(env), func(a string) { addr <- a }) }()

	var listen string
	select {
	case listen = <-addr:
	case err := <-done:
		t.Fatalf("serve stopped: %v\n%s", err, logs.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("serve not ready\n%s", logs.String())
	}
	if err := health.Probe(ctx, "http://"+listen+health.Path); err != nil {
		t.Fatalf("health: %v", err)
	}
	// The queue API is served and refuses an unsigned request.
	resp, err := http.Post("http://"+listen+"/api/jobs", "application/json", strings.NewReader(`{"job":"ping","request_id":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned queue request: %d, want 401", resp.StatusCode)
	}
	// Only /healthz and /api/jobs are served on the public listener; the
	// operator routes live on loopback only (PX-NFR-05).
	for _, path := range []string{"/", "/admin/runs", "/debug/pprof/", "/metrics", "/api", "/healthz/x"} {
		resp, err := http.Get("http://" + listen + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("public %s answered %d, want 404", path, resp.StatusCode)
		}
	}

	// Make ping due now; the scheduler ticks every second.
	if _, err := pool.Exec(ctx, `UPDATE job SET next_due_at = now() - interval '1 second' WHERE name = 'ping'`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var state string
		err := pool.QueryRow(ctx, `SELECT state FROM run ORDER BY due_at DESC LIMIT 1`).Scan(&state)
		if err == nil && state == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run state %q (%v)\n%s", state, err, logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A queued job, sent with paradox enqueue as the system, runs too
	// (how it works, Flow B).
	sysKey := filepath.Join(t.TempDir(), "localhost.key")
	os.WriteFile(sysKey, []byte(sysPriv+"\n"), 0o600)
	code, out, errOut := runArgs(t, noEnv, "enqueue", "-url", "http://"+listen, "-system", "localhost", "-key", sysKey, "-job", "ping", "-data", `{"learner":"secret-marker-7731"}`)
	if code != 0 || !strings.Contains(out, `"state": "queued"`) {
		t.Fatalf("enqueue: exit %d out %q err %q", code, out, errOut)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		var state string
		err := pool.QueryRow(ctx, `SELECT state FROM run WHERE source = 'queue'`).Scan(&state)
		if err == nil && state == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued run state %q (%v)\n%s", state, err, logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The operator sees both runs through the loopback admin endpoint.
	code, out, errOut = runArgs(t, envFrom(env), "runs", "-json")
	if code != 0 || strings.Count(out, `"state": "succeeded"`) != 2 {
		t.Fatalf("paradox runs: exit %d out %s err %s", code, out, errOut)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("serve did not stop")
	}
	out = logs.String()
	if !strings.Contains(out, `"msg":"paradox started"`) || !strings.Contains(out, `"msg":"paradox stopping"`) {
		t.Fatalf("missing start or stop log:\n%s", out)
	}
	if strings.Contains(out, "ed25519-priv:") || strings.Contains(out, strings.TrimPrefix(privText, "ed25519-priv:")) {
		t.Fatal("private key reached the log")
	}
	if strings.Contains(out, "secret-marker-7731") {
		t.Fatal("job data reached the log (PX-NFR-06)")
	}
}

func TestServeRefusesBadAdapterURLsWithoutEchoingThem(t *testing.T) {
	_, privText, _ := sign.GenerateKey()
	err := serve(context.Background(), &bytes.Buffer{}, envFrom(map[string]string{
		"PARADOX_PRIVATE_KEY":   privText,
		"PARADOX_SABLIER_URL":   "http://waker.example:10000/?x=1",
		"PARADOX_HEARTBEAT_URL": "https://monitor.example/api/push/tok3n-secret",
	}), nil)
	if err == nil || !strings.Contains(err.Error(), "PARADOX_SABLIER_URL") || strings.Contains(err.Error(), "tok3n-secret") {
		t.Fatalf("got %v", err)
	}
}

func TestServeRefusesBadSettingsAndFolder(t *testing.T) {
	if err := serve(context.Background(), &bytes.Buffer{}, envFrom(nil), nil); err == nil || !strings.Contains(err.Error(), "PARADOX_PRIVATE_KEY") {
		t.Fatalf("missing key: %v", err)
	}
	_, privText, _ := sign.GenerateKey()
	err := serve(context.Background(), &bytes.Buffer{}, envFrom(map[string]string{
		"PARADOX_PRIVATE_KEY": privText,
		"PARADOX_SYSTEMS_DIR": t.TempDir(),
	}), nil)
	if err == nil || !strings.Contains(err.Error(), "holds no systems") {
		t.Fatalf("empty folder: %v", err)
	}
}

func TestServeTakesNoArguments(t *testing.T) {
	if code, _, _ := runArgs(t, noEnv, "serve", "extra"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
