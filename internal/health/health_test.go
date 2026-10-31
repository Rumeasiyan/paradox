package health

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func serve(t *testing.T, h http.Handler, method string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, Path, nil))
	return rec
}

func TestHandlerHealthy(t *testing.T) {
	h := Handler(func(context.Context) error { return nil }, nil)
	rec := serve(t, h, http.MethodGet)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Fatalf("body = %q, want ok", got)
	}
}

func TestHandlerUnhealthyLogsReasonButDoesNotSendIt(t *testing.T) {
	const reason = "dial tcp db.example.com:5432: secret-detail"
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	h := Handler(func(context.Context) error { return errors.New(reason) }, logger)
	rec := serve(t, h, http.MethodGet)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if strings.TrimSpace(body) != "unhealthy" {
		t.Fatalf("body = %q, want unhealthy", body)
	}
	if strings.Contains(body, "secret-detail") || strings.Contains(body, "example.com") {
		t.Fatalf("body leaks the failure reason: %q", body)
	}
	if !strings.Contains(logs.String(), "secret-detail") {
		t.Fatalf("reason not logged; log = %q", logs.String())
	}
}

func TestHandlerRefusesOtherMethods(t *testing.T) {
	called := false
	h := Handler(func(context.Context) error { called = true; return nil }, nil)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		rec := serve(t, h, m)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", m, rec.Code)
		}
		if rec.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s: Allow = %q, want GET", m, rec.Header().Get("Allow"))
		}
	}
	if called {
		t.Fatal("check ran for a refused method")
	}
}

func TestHandlerOtherPathsNotFound(t *testing.T) {
	h := Handler(func(context.Context) error { return nil }, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProbeHealthy(t *testing.T) {
	srv := httptest.NewServer(Handler(func(context.Context) error { return nil }, nil))
	defer srv.Close()
	if err := Probe(context.Background(), srv.URL+Path); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestProbeUnhealthy(t *testing.T) {
	srv := httptest.NewServer(Handler(func(context.Context) error { return errors.New("down") }, nil))
	defer srv.Close()
	err := Probe(context.Background(), srv.URL+Path)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Probe err = %v, want one naming 503", err)
	}
}

func TestProbeOnlyCounts200(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusFound, http.StatusNotFound, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if code == http.StatusFound {
				w.Header().Set("Location", "/elsewhere")
			}
			w.WriteHeader(code)
		}))
		if err := Probe(context.Background(), srv.URL+Path); err == nil {
			t.Errorf("status %d counted as healthy", code)
		}
		srv.Close()
	}
}

func TestProbeTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	err := probe(context.Background(), srv.URL+Path, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("probe err = %v, want a timeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("probe took %s; the limit was not applied", time.Since(start))
	}
}

func TestProbeConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	err = Probe(context.Background(), "http://"+addr+Path)
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("Probe err = %v, want not reachable", err)
	}
}

func TestProbeBadURL(t *testing.T) {
	if err := Probe(context.Background(), "http://bad host/"); err == nil {
		t.Fatal("Probe accepted a malformed address")
	}
}

func TestProbeLimitIsLongerThanDBLimit(t *testing.T) {
	// Otherwise a slow database would show as a probe timeout instead of 503.
	if ProbeTimeout <= DBTimeout {
		t.Fatalf("ProbeTimeout %s must be longer than DBTimeout %s", ProbeTimeout, DBTimeout)
	}
}

func TestLoopbackURL(t *testing.T) {
	tests := []struct {
		listen, want string
	}{
		{":8080", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0:8080", "http://127.0.0.1:8080/healthz"},
		{"[::]:8080", "http://127.0.0.1:8080/healthz"},
		{"127.0.0.1:9090", "http://127.0.0.1:9090/healthz"},
		{"paradox.example.com:7000", "http://127.0.0.1:7000/healthz"},
	}
	for _, tc := range tests {
		got, err := LoopbackURL(tc.listen)
		if err != nil {
			t.Errorf("LoopbackURL(%q): %v", tc.listen, err)
			continue
		}
		if got != tc.want {
			t.Errorf("LoopbackURL(%q) = %q, want %q", tc.listen, got, tc.want)
		}
	}
}

func TestLoopbackURLRejectsBadListen(t *testing.T) {
	for _, listen := range []string{"", "8080", ":", ":http", ":0", ":65536", ":-1", "::8080"} {
		if got, err := LoopbackURL(listen); err == nil {
			t.Errorf("LoopbackURL(%q) = %q, want an error", listen, got)
		} else if !strings.Contains(err.Error(), "PARADOX_LISTEN") {
			t.Errorf("LoopbackURL(%q) error %q does not name the setting", listen, err)
		}
	}
}

func TestDBCheckAgainstDatabase(t *testing.T) {
	url := os.Getenv("PARADOX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PARADOX_TEST_DATABASE_URL not set; start dev/compose.yaml to run database tests")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := DBCheck(pool)(context.Background()); err != nil {
		t.Fatalf("DBCheck: %v", err)
	}
}

func TestDBCheckClosedPool(t *testing.T) {
	// pgxpool.New does not connect, so this needs no database.
	pool, err := pgxpool.New(context.Background(), "postgres://paradox@127.0.0.1:1/paradox")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	err = DBCheck(pool)(context.Background())
	if err == nil || !strings.Contains(err.Error(), "database not reachable") {
		t.Fatalf("DBCheck on a closed pool = %v, want database not reachable", err)
	}
}

func TestDBCheckNilPool(t *testing.T) {
	if err := DBCheck(nil)(context.Background()); err == nil {
		t.Fatal("DBCheck(nil) passed")
	}
}

func TestDBCheckUnhealthyThroughHandler(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://paradox@127.0.0.1:1/paradox")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	rec := serve(t, Handler(DBCheck(pool), nil), http.MethodGet)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
