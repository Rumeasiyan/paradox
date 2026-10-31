package report

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// testToken stands in for the secret part of a push URL. It must never
// appear in an error or a log line.
const testToken = "s3cr3tT0ken"

// pushRequest is one request the fake monitor received.
type pushRequest struct {
	path     string
	rawQuery string
	status   string
	msg      string
	extra    string // the "keep" parameter, if any
}

// fakeKuma plays an Uptime Kuma push endpoint: it records each GET and
// answers with code.
type fakeKuma struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []pushRequest
	code int
}

func newKuma(t *testing.T) *fakeKuma {
	t.Helper()
	k := &fakeKuma{code: http.StatusOK}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		q := r.URL.Query()
		k.mu.Lock()
		k.reqs = append(k.reqs, pushRequest{
			path:     r.URL.Path,
			rawQuery: r.URL.RawQuery,
			status:   q.Get("status"),
			msg:      q.Get("msg"),
			extra:    q.Get("keep"),
		})
		code := k.code
		k.mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(k.srv.Close)
	return k
}

// url returns a push URL on the fake for one monitor, with the token in the
// path as Kuma does and an extra parameter that must be kept.
func (k *fakeKuma) url(monitor string) string {
	return k.srv.URL + "/api/push/" + monitor + testToken + "?keep=yes"
}

func (k *fakeKuma) requests() []pushRequest {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]pushRequest(nil), k.reqs...)
}

func (k *fakeKuma) setCode(c int) {
	k.mu.Lock()
	k.code = c
	k.mu.Unlock()
}

func mustPush(t *testing.T, hb, fl string, logger *slog.Logger) *Push {
	t.Helper()
	p, err := NewPush(hb, fl, nil, logger)
	if err != nil {
		t.Fatalf("NewPush: %v", err)
	}
	return p
}

// assertNoSecret fails if s holds the token or the fake's address.
func assertNoSecret(t *testing.T, what, s string, k *fakeKuma) {
	t.Helper()
	if strings.Contains(s, testToken) {
		t.Errorf("%s contains the token: %q", what, s)
	}
	if k != nil && strings.Contains(s, strings.TrimPrefix(k.srv.URL, "http://")) {
		t.Errorf("%s contains the monitor address: %q", what, s)
	}
	if strings.Contains(s, "/api/push") || strings.Contains(s, "keep=") {
		t.Errorf("%s contains part of the URL: %q", what, s)
	}
}

func TestPushHeartbeatSendsUpAndKeepsQuery(t *testing.T) {
	k := newKuma(t)
	p := mustPush(t, k.url("hb"), "", nil)
	if err := p.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	reqs := k.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.path != "/api/push/hb"+testToken {
		t.Errorf("path = %q, token path not kept", r.path)
	}
	if r.status != "up" || r.msg != "OK" || r.extra != "yes" {
		t.Errorf("got status=%q msg=%q keep=%q, want up, OK, yes", r.status, r.msg, r.extra)
	}
}

func TestPushHeartbeatOverridesExistingStatus(t *testing.T) {
	k := newKuma(t)
	p := mustPush(t, k.srv.URL+"/api/push/hb"+testToken+"?status=down&msg=x&ping=", "", nil)
	if err := p.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	r := k.requests()[0]
	if r.status != "up" || r.msg != "OK" {
		t.Errorf("got status=%q msg=%q, want up, OK", r.status, r.msg)
	}
	if strings.Count(r.rawQuery, "status=") != 1 {
		t.Errorf("status sent more than once: %q", r.rawQuery)
	}
}

func TestPushFailureSendsDownWithMessage(t *testing.T) {
	k := newKuma(t)
	p := mustPush(t, "", k.url("fl"), nil)
	f := NewFailure("citadel", "nightly-report", "run-42", 4, "connection refused & reset")
	if err := p.Failure(context.Background(), f); err != nil {
		t.Fatalf("Failure: %v", err)
	}
	reqs := k.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	want := "citadel/nightly-report run run-42 failed after 4 attempts: connection refused & reset"
	if r.status != "down" || r.msg != want || r.extra != "yes" {
		t.Errorf("got status=%q msg=%q keep=%q, want down, %q, yes", r.status, r.msg, r.extra, want)
	}
	// Encoded: spaces as %20 and & escaped, so msg cannot spill into other
	// parameters.
	if strings.Contains(r.rawQuery, "+") || !strings.Contains(r.rawQuery, "%20") || !strings.Contains(r.rawQuery, "%26") {
		t.Errorf("msg not encoded as expected: %q", r.rawQuery)
	}
}

func TestPushFailureMessageIsCutAndPlain(t *testing.T) {
	k := newKuma(t)
	p := mustPush(t, "", k.url("fl"), nil)
	// Hand-built, so Push itself must truncate; multi-byte and new lines too.
	long := "line one\nline\ttwo " + strings.Repeat("අ", 5000)
	f := Failure{System: "citadel", Job: "j", RunID: "r", Attempts: 2, LastError: long}
	if err := p.Failure(context.Background(), f); err != nil {
		t.Fatalf("Failure: %v", err)
	}
	msg := k.requests()[0].msg
	if n := utf8.RuneCountInString(msg); n != MaxPushMessage {
		t.Errorf("msg is %d characters, want %d", n, MaxPushMessage)
	}
	if !utf8.ValidString(msg) || strings.ContainsAny(msg, "\n\t\r") {
		t.Errorf("msg is not one plain line: %q", msg)
	}
	if !strings.HasPrefix(msg, "citadel/j run r failed after 2 attempts: line one line two ") {
		t.Errorf("msg = %q", msg)
	}
}

func TestFailureMessageShort(t *testing.T) {
	got := FailureMessage(NewFailure("citadel", "j", "r", 1, ""))
	if got != "citadel/j run r failed after 1 attempts:" {
		t.Errorf("got %q", got)
	}
}

func TestPushHeartbeatAlsoRecoversFailureMonitor(t *testing.T) {
	k := newKuma(t)
	p := mustPush(t, k.url("hb"), k.url("fl"), nil)
	if err := p.Failure(context.Background(), NewFailure("citadel", "j", "r", 3, "boom")); err != nil {
		t.Fatalf("Failure: %v", err)
	}
	if err := p.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	reqs := k.requests()
	if len(reqs) != 3 {
		t.Fatalf("got %d requests, want 3: %+v", len(reqs), reqs)
	}
	want := []struct{ path, status string }{
		{"/api/push/fl" + testToken, "down"},
		{"/api/push/hb" + testToken, "up"},
		{"/api/push/fl" + testToken, "up"},
	}
	for i, w := range want {
		if reqs[i].path != w.path || reqs[i].status != w.status {
			t.Errorf("request %d = %s status=%s, want %s status=%s", i, reqs[i].path, reqs[i].status, w.path, w.status)
		}
	}
}

func TestPushHeartbeatWithOnlyFailureURL(t *testing.T) {
	k := newKuma(t)
	logger, buf := newLogger(slog.LevelDebug)
	p := mustPush(t, "", k.url("fl"), logger)
	if err := p.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	reqs := k.requests()
	if len(reqs) != 1 || reqs[0].status != "up" || reqs[0].path != "/api/push/fl"+testToken {
		t.Errorf("got %+v, want one up push to the failure monitor", reqs)
	}
	if !strings.Contains(buf.String(), `"msg":"heartbeat"`) {
		t.Errorf("heartbeat not logged: %q", buf.String())
	}
}

func TestPushEmptyURLsFallBackToLog(t *testing.T) {
	logger, buf := newLogger(slog.LevelDebug)
	p := mustPush(t, "", "", logger)
	if err := p.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := p.Failure(context.Background(), NewFailure("citadel", "j", "run-7", 4, "boom")); err != nil {
		t.Fatalf("Failure: %v", err)
	}
	recs := buf.records(t)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2: %s", len(recs), buf.String())
	}
	if recs[0]["level"] != "DEBUG" || recs[0]["msg"] != "heartbeat" {
		t.Errorf("first record = %v, want a DEBUG heartbeat", recs[0])
	}
	if recs[1]["level"] != "ERROR" || recs[1]["run_id"] != "run-7" || recs[1]["last_error"] != "boom" {
		t.Errorf("second record = %v, want the failure at ERROR", recs[1])
	}
}

func TestPushAcceptsAny2xx(t *testing.T) {
	k := newKuma(t)
	p := mustPush(t, k.url("hb"), k.url("fl"), nil)
	for _, c := range []int{200, 201, 202, 204, 299} {
		k.setCode(c)
		if err := p.Heartbeat(context.Background()); err != nil {
			t.Errorf("code %d: Heartbeat: %v", c, err)
		}
		if err := p.Failure(context.Background(), NewFailure("c", "j", "r", 1, "e")); err != nil {
			t.Errorf("code %d: Failure: %v", c, err)
		}
	}
}

func TestPushNon2xxErrorsWithoutURL(t *testing.T) {
	k := newKuma(t)
	logger, buf := newLogger(slog.LevelDebug)
	p := mustPush(t, k.url("hb"), k.url("fl"), logger)
	for _, c := range []int{302, 404, 500} {
		k.setCode(c)
		herr := p.Heartbeat(context.Background())
		if herr == nil {
			t.Fatalf("code %d: Heartbeat gave no error", c)
		}
		for _, want := range []string{"heartbeat push", "failure recovery push", "answered " + strconv.Itoa(c)} {
			if !strings.Contains(herr.Error(), want) {
				t.Errorf("code %d: heartbeat error %q lacks %q", c, herr, want)
			}
		}
		assertNoSecret(t, "heartbeat error", herr.Error(), k)

		ferr := p.Failure(context.Background(), NewFailure("citadel", "j", "r", 1, "e"))
		if ferr == nil {
			t.Fatalf("code %d: Failure gave no error", c)
		}
		if !strings.HasPrefix(ferr.Error(), "failure push: monitor answered "+strconv.Itoa(c)) {
			t.Errorf("code %d: failure error = %q", c, ferr)
		}
		assertNoSecret(t, "failure error", ferr.Error(), k)
	}
	// A failed failure push is logged so the report is not lost, and the
	// log has no URL either.
	if !strings.Contains(buf.String(), `"run_id":"r"`) {
		t.Errorf("failed failure push was not logged: %s", buf.String())
	}
	assertNoSecret(t, "log", buf.String(), k)
}

func TestPushUnreachableErrorsWithoutURL(t *testing.T) {
	// A port that was open and is now closed: connection refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	raw := "http://" + addr + "/api/push/" + testToken + "?keep=yes"
	logger, buf := newLogger(slog.LevelDebug)
	p := mustPush(t, raw, raw, logger)

	herr := p.Heartbeat(context.Background())
	ferr := p.Failure(context.Background(), NewFailure("citadel", "j", "r", 1, "e"))
	for name, e := range map[string]error{"heartbeat": herr, "failure": ferr} {
		if e == nil {
			t.Fatalf("%s: no error for an unreachable monitor", name)
		}
		var ue interface{ URL() string }
		if errors.As(e, &ue) {
			t.Errorf("%s: error still wraps a URL", name)
		}
		if strings.Contains(e.Error(), addr) {
			t.Errorf("%s: error contains the address: %q", name, e)
		}
		assertNoSecret(t, name+" error", e.Error(), nil)
		if !strings.Contains(e.Error(), name+" push") {
			t.Errorf("%s: error %q does not say which push failed", name, e)
		}
	}
	if strings.Contains(buf.String(), addr) {
		t.Errorf("log contains the address: %s", buf.String())
	}
	assertNoSecret(t, "log", buf.String(), nil)
}

func TestPushTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	p := mustPush(t, srv.URL+"/api/push/"+testToken, "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := p.Heartbeat(ctx)
	if err == nil || !strings.Contains(err.Error(), "heartbeat push: timed out") {
		t.Errorf("got %v, want a heartbeat timeout", err)
	}
	if err != nil {
		assertNoSecret(t, "timeout error", err.Error(), nil)
	}
}

func TestNewPushValidation(t *testing.T) {
	bad := []struct {
		name, raw, want string
	}{
		{"no scheme", "monitor.example/api/push/" + testToken, "http:// or https://"},
		{"ftp", "ftp://monitor.example/api/push/" + testToken, "http:// or https://"},
		{"no host", "https:///api/push/" + testToken, "no host"},
		{"bad escape", "https://monitor.example/api/push/" + testToken + "%zz", "not a valid URL"},
		{"control char", "https://monitor.example/" + testToken + "\n", "not a valid URL"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewPush(c.raw, "", nil, nil)
			if err == nil || !strings.Contains(err.Error(), "heartbeat push URL") || !strings.Contains(err.Error(), c.want) {
				t.Errorf("heartbeat: got %v, want an error with %q", err, c.want)
			}
			if err != nil {
				assertNoSecret(t, "error", err.Error(), nil)
			}
			_, err = NewPush("", c.raw, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "failure push URL") {
				t.Errorf("failure: got %v, want a failure push URL error", err)
			}
			if err != nil {
				assertNoSecret(t, "error", err.Error(), nil)
			}
		})
	}
	for _, good := range []string{"", "http://monitor.example/api/push/x", "https://monitor.example:3001/api/push/x?status=up&msg=OK&ping="} {
		if _, err := NewPush(good, good, nil, nil); err != nil {
			t.Errorf("NewPush(%q): %v", good, err)
		}
	}
}

func TestPushConcurrentUse(t *testing.T) {
	k := newKuma(t)
	p := mustPush(t, k.url("hb"), k.url("fl"), nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := p.Heartbeat(context.Background()); err != nil {
				t.Errorf("Heartbeat: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := p.Failure(context.Background(), NewFailure("citadel", "j", "r", 1, "e")); err != nil {
				t.Errorf("Failure: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := len(k.requests()); got != 60 {
		t.Errorf("got %d requests, want 60", got)
	}
}
