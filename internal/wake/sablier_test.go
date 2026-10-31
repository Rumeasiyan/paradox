package wake

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSession = 3 * time.Minute

// fakeSablier is an httptest server playing Sablier's blocking strategy. It
// records every request; answer decides the reply.
type fakeSablier struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	answer   http.HandlerFunc
}

func newFakeSablier(t *testing.T, answer http.HandlerFunc) *fakeSablier {
	t.Helper()
	f := &fakeSablier{answer: answer}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		f.mu.Unlock()
		f.answer(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSablier) Requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.requests...)
}

func ready(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(sablierStatusHeader, "ready")
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"session":{"status":"ready"}}`)
}

func problem(status int, detail string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"type":"about:blank","title":%q,"status":%d,"detail":%q,"instance":%q}`,
			http.StatusText(status), status, detail, r.URL.RequestURI())
	}
}

// honourTimeout behaves like Sablier for a group that never gets ready: it
// waits for the timeout it was sent, then answers 504.
func honourTimeout(w http.ResponseWriter, r *http.Request) {
	d, err := time.ParseDuration(r.URL.Query().Get("timeout"))
	if err != nil {
		problem(http.StatusBadRequest, "bad timeout")(w, r)
		return
	}
	select {
	case <-time.After(d):
		problem(http.StatusGatewayTimeout, "session was not ready before the timeout")(w, r)
	case <-r.Context().Done():
	}
}

func newTestSablier(t *testing.T, baseURL string) *Sablier {
	t.Helper()
	s, err := NewSablier(baseURL, testSession, nil)
	if err != nil {
		t.Fatalf("NewSablier: %v", err)
	}
	return s
}

// assertSafeError checks an error names the system and does not leak the
// request URL or its credentials.
func assertSafeError(t *testing.T, err error, leaks ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, citadel) {
		t.Fatalf("err %q does not name the system", msg)
	}
	for _, l := range append(leaks, sablierBlockingPath, "session_duration") {
		if strings.Contains(msg, l) {
			t.Fatalf("err %q leaks %q", msg, l)
		}
	}
}

func TestSablierReadyAtOnce(t *testing.T) {
	f := newFakeSablier(t, ready)
	s := newTestSablier(t, f.URL)
	if err := WakeWithin(context.Background(), s, citadel, 2*time.Minute); err != nil {
		t.Fatalf("WakeWithin: %v", err)
	}
	if n := len(f.Requests()); n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
}

// PX-RUN-01, Annex B link 7: the exact request sent to Sablier.
func TestSablierWakeRequest(t *testing.T) {
	f := newFakeSablier(t, ready)
	s := newTestSablier(t, f.URL+"/sablier/") // a base path, trailing slash
	const limit = 2 * time.Minute
	if err := WakeWithin(context.Background(), s, citadel, limit); err != nil {
		t.Fatalf("WakeWithin: %v", err)
	}
	r := f.Requests()[0]
	if r.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", r.Method)
	}
	if r.URL.Path != "/sablier/api/strategies/blocking" {
		t.Fatalf("path = %q", r.URL.Path)
	}
	q := r.URL.Query()
	if len(q) != 3 {
		t.Fatalf("query = %v, want exactly group, session_duration and timeout", q)
	}
	if got := q["group"]; len(got) != 1 || got[0] != citadel {
		t.Fatalf("group = %v, want [%s]", got, citadel)
	}
	if q.Has("names") {
		t.Fatal("names sent with group; Sablier refuses both")
	}
	if got := q.Get("session_duration"); got != "3m0s" {
		t.Fatalf("session_duration = %q, want 3m0s", got)
	}
	timeout, err := time.ParseDuration(q.Get("timeout"))
	if err != nil {
		t.Fatalf("timeout %q is not a Go duration: %v", q.Get("timeout"), err)
	}
	// The timeout is the time left, less at most a second for the answer.
	if timeout > limit || timeout < limit-maxTimeoutMargin-time.Second {
		t.Fatalf("timeout = %s, want just under %s", timeout, limit)
	}
	if r.Header.Get("Authorization") != "" {
		t.Fatal("Authorization sent without credentials in the URL")
	}
}

func TestSablierWakeWithoutDeadlineLeavesTimeoutOut(t *testing.T) {
	f := newFakeSablier(t, ready)
	s := newTestSablier(t, f.URL)
	if err := s.Wake(context.Background(), citadel); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if q := f.Requests()[0].URL.Query(); q.Has("timeout") {
		t.Fatalf("timeout = %q sent without a deadline", q.Get("timeout"))
	}
}

func TestSablierReadyAfterDelay(t *testing.T) {
	const delay = 50 * time.Millisecond
	f := newFakeSablier(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
			ready(w, r)
		case <-r.Context().Done():
		}
	})
	s := newTestSablier(t, f.URL)
	start := time.Now()
	if err := WakeWithin(context.Background(), s, citadel, 5*time.Second); err != nil {
		t.Fatalf("WakeWithin: %v", err)
	}
	if d := time.Since(start); d < delay {
		t.Fatalf("returned after %s, before Sablier said ready", d)
	}
}

// Sablier answers 504 at the timeout it was sent, before PARADOX's limit.
func TestSablierNeverReadyIsTimeout(t *testing.T) {
	f := newFakeSablier(t, honourTimeout)
	s := newTestSablier(t, f.URL)
	const limit = 200 * time.Millisecond
	start := time.Now()
	err := WakeWithin(context.Background(), s, citadel, limit)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	assertSafeError(t, err, f.URL)
	if d := time.Since(start); d > limit+time.Second {
		t.Fatalf("took %s, the limit was not honoured", d)
	}
}

// A Sablier that never answers is cut off at the limit.
func TestSablierHangingIsTimeout(t *testing.T) {
	f := newFakeSablier(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	s := newTestSablier(t, f.URL)
	const limit = 100 * time.Millisecond
	start := time.Now()
	err := WakeWithin(context.Background(), s, citadel, limit)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > limit+time.Second {
		t.Fatalf("took %s, the limit was not honoured", d)
	}
}

func TestSablierNotReadyAnswerIsTimeout(t *testing.T) {
	f := newFakeSablier(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(sablierStatusHeader, "not-ready")
		fmt.Fprint(w, `{}`)
	})
	s := newTestSablier(t, f.URL)
	err := WakeWithin(context.Background(), s, citadel, time.Second)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestSablierOKWithoutStatusHeaderIsNotReady(t *testing.T) {
	f := newFakeSablier(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{}`)
	})
	s := newTestSablier(t, f.URL)
	err := WakeWithin(context.Background(), s, citadel, time.Second)
	if err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want a plain error", err)
	}
	assertSafeError(t, err, f.URL)
}

// A ready answer that arrives after the deadline is not reported as ready.
func TestSablierReadyAfterDeadlineIsNotReady(t *testing.T) {
	f := newFakeSablier(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // ignores the request context
		ready(w, r)
	})
	s := newTestSablier(t, f.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.Wake(ctx, citadel); err == nil {
		t.Fatal("Wake returned nil after the deadline")
	}
}

func TestSablierExpiredContextSendsNothing(t *testing.T) {
	f := newFakeSablier(t, ready)
	s := newTestSablier(t, f.URL)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := s.Wake(ctx, citadel); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("requests = %d, want none", n)
	}
}

func TestSablierCallerCancelledIsNotTimeout(t *testing.T) {
	f := newFakeSablier(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	s := newTestSablier(t, f.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := WakeWithin(ctx, s, citadel, 5*time.Second)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want context.Canceled only", err)
	}
}

func TestSablierUnreachable(t *testing.T) {
	f := newFakeSablier(t, ready)
	base := f.URL
	f.Close() // nothing listens there any more
	u, _ := url.Parse(base)
	u.User = url.UserPassword("paradox", "s3cret-pass")
	s := newTestSablier(t, u.String())
	err := WakeWithin(context.Background(), s, citadel, 5*time.Second)
	if err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want a plain error", err)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err %q does not say Sablier is unreachable", err)
	}
	assertSafeError(t, err, "s3cret-pass", "paradox:")
}

func TestSablierErrorAnswers(t *testing.T) {
	for _, tc := range []struct {
		status int
		detail string
	}{
		{http.StatusBadRequest, "'names' or 'group' query parameter must be set"},
		{http.StatusNotFound, "group citadel not found"},
		{http.StatusInternalServerError, "docker went away"},
		{http.StatusServiceUnavailable, "request cancelled"},
		{http.StatusBadGateway, ""},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := newFakeSablier(t, problem(tc.status, tc.detail))
			s := newTestSablier(t, f.URL)
			err := WakeWithin(context.Background(), s, citadel, 5*time.Second)
			if err == nil || errors.Is(err, ErrTimeout) {
				t.Fatalf("err = %v, want a plain error", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(tc.status)) {
				t.Fatalf("err %q does not name the status", err)
			}
			if tc.detail != "" && !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("err %q does not quote Sablier's detail", err)
			}
			assertSafeError(t, err, f.URL)
		})
	}
}

func TestSablierErrorBodyNotProblem(t *testing.T) {
	f := newFakeSablier(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "<html>"+strings.Repeat("x", 100<<10)+"</html>")
	})
	s := newTestSablier(t, f.URL)
	err := s.Wake(context.Background(), citadel)
	if err == nil || strings.Contains(err.Error(), "<html>") {
		t.Fatalf("err = %v, want the status without the body", err)
	}
	assertSafeError(t, err, f.URL)
}

func TestSablierCredentialsSentAsBasicAuth(t *testing.T) {
	f := newFakeSablier(t, problem(http.StatusInternalServerError, "boom"))
	u, _ := url.Parse(f.URL)
	u.User = url.UserPassword("paradox", "s3cret-pass")
	s := newTestSablier(t, u.String())
	err := s.Wake(context.Background(), citadel)
	assertSafeError(t, err, "s3cret-pass", "paradox:")
	user, pass, ok := f.Requests()[0].BasicAuth()
	if !ok || user != "paradox" || pass != "s3cret-pass" {
		t.Fatalf("basic auth = %q, %q, %v", user, pass, ok)
	}
}

// PX-RUN-02: keep awake renews the same session with a short timeout.
func TestSablierKeepAwakeRenewsSession(t *testing.T) {
	f := newFakeSablier(t, ready)
	s := newTestSablier(t, f.URL)
	if err := WakeWithin(context.Background(), s, citadel, time.Minute); err != nil {
		t.Fatalf("WakeWithin: %v", err)
	}
	if err := s.KeepAwake(context.Background(), citadel); err != nil {
		t.Fatalf("KeepAwake: %v", err)
	}
	reqs := f.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	wake, keep := reqs[0].URL, reqs[1].URL
	if keep.Path != wake.Path || keep.Path != sablierBlockingPath {
		t.Fatalf("keep-awake path = %q, wake path = %q", keep.Path, wake.Path)
	}
	q := keep.Query()
	if q.Get("group") != citadel {
		t.Fatalf("group = %q", q.Get("group"))
	}
	if q.Get("session_duration") != wake.Query().Get("session_duration") {
		t.Fatalf("keep-awake session %q, wake session %q",
			q.Get("session_duration"), wake.Query().Get("session_duration"))
	}
	if q.Get("timeout") != keepAwakeTimeout.String() {
		t.Fatalf("timeout = %q, want %s", q.Get("timeout"), keepAwakeTimeout)
	}
}

func TestSablierKeepAwakeFailure(t *testing.T) {
	f := newFakeSablier(t, problem(http.StatusNotFound, "group citadel not found"))
	s := newTestSablier(t, f.URL)
	err := s.KeepAwake(context.Background(), citadel)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the 404", err)
	}
	assertSafeError(t, err, f.URL)
}

// KeepAwakeLoop drives the adapter: renewals reach Sablier.
func TestSablierWithKeepAwakeLoop(t *testing.T) {
	f := newFakeSablier(t, ready)
	s := newTestSablier(t, f.URL)
	logger, _ := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		KeepAwakeLoop(ctx, s, citadel, 5*time.Millisecond, logger)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(f.Requests()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d renewals", len(f.Requests()))
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestNewSablierValidation(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		session   time.Duration
	}{
		{"empty", "", testSession},
		{"no scheme", "sablier.example:10000", testSession},
		{"ftp", "ftp://sablier.example", testSession},
		{"no host", "http://", testSession},
		{"port only", "http://:10000", testSession},
		{"query", "http://sablier.example/?x=1", testSession},
		{"empty query", "http://sablier.example/?", testSession},
		{"fragment", "http://sablier.example/#top", testSession},
		{"not a URL", "http://user:s3cret-pass@sablier.example:port", testSession},
		{"zero session", "http://sablier.example", 0},
		{"negative session", "http://sablier.example", -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewSablier(tc.url, tc.session, nil)
			if err == nil {
				t.Fatalf("NewSablier(%q, %s) = %+v, want an error", tc.url, tc.session, s)
			}
			if strings.Contains(err.Error(), "s3cret-pass") {
				t.Fatalf("err %q leaks the password", err)
			}
		})
	}
	for _, ok := range []string{
		"http://sablier.example:10000",
		"https://sablier.example",
		"http://sablier.example/base/",
		"http://user:pass@sablier.example",
	} {
		if _, err := NewSablier(ok, testSession, nil); err != nil {
			t.Fatalf("NewSablier(%q): %v", ok, err)
		}
	}
}

func TestNewSablierUsesGivenClient(t *testing.T) {
	f := newFakeSablier(t, ready)
	c := &http.Client{}
	s, err := NewSablier(f.URL, testSession, c)
	if err != nil {
		t.Fatalf("NewSablier: %v", err)
	}
	if s.client != c {
		t.Fatal("the given client was not used")
	}
}

// Run with -race: many wakes and keep-awakes on one Sablier at once.
func TestSablierConcurrentUse(t *testing.T) {
	f := newFakeSablier(t, ready)
	s := newTestSablier(t, f.URL)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			system := fmt.Sprintf("system-%d", i%4)
			errs <- WakeWithin(context.Background(), s, system, 5*time.Second)
			errs <- s.KeepAwake(context.Background(), system)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(f.Requests()); got != 2*n {
		t.Fatalf("requests = %d, want %d", got, 2*n)
	}
}
