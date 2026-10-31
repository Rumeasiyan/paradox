package testsystem

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rumeasiyan/paradox/internal/jobcall"
	"github.com/Rumeasiyan/paradox/sign"
)

// keyPair makes a fresh key pair inside the test; no key is ever stored.
func keyPair(t *testing.T) (pubText string, pub ed25519.PublicKey, priv ed25519.PrivateKey) {
	t.Helper()
	pubText, privText, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if pub, err = sign.ParsePublicKey(pubText); err != nil {
		t.Fatal(err)
	}
	if priv, err = sign.ParsePrivateKey(privText); err != nil {
		t.Fatal(err)
	}
	return pubText, pub, priv
}

// safeBuffer lets the handler log from server goroutines while the test reads.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fixture struct {
	t    *testing.T
	srv  *httptest.Server
	priv ed25519.PrivateKey
	logs *safeBuffer
}

func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	t.Helper()
	_, pub, priv := keyPair(t)
	cfg := Config{
		Listen:      DefaultListen,
		ParadoxKeys: []ed25519.PublicKey{pub},
		FailJobs:    map[string]bool{},
		SlowJobs:    map[string]time.Duration{},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	logs := &safeBuffer{}
	srv := httptest.NewServer(New(cfg, slog.New(slog.NewJSONHandler(logs, nil))))
	t.Cleanup(srv.Close)
	return &fixture{t: t, srv: srv, priv: priv, logs: logs}
}

func callBody(t *testing.T, job, runID string, attempt int) []byte {
	t.Helper()
	b, err := json.Marshal(jobcall.Call{
		RunID:   runID,
		Job:     job,
		Attempt: attempt,
		DueAt:   time.Date(2026, 11, 1, 1, 30, 0, 0, time.UTC),
		Data:    json.RawMessage(`{"secret":"personal-data-here"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *fixture) request(path string, body []byte) *http.Request {
	f.t.Helper()
	r, err := http.NewRequest(http.MethodPost, f.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	return r
}

func (f *fixture) sign(r *http.Request, body []byte, s sign.Signer) {
	f.t.Helper()
	if s.Key == nil {
		s.Key = f.priv
	}
	if s.KeyID == "" {
		s.KeyID = sign.ParadoxKeyID
	}
	if s.Direction == "" {
		s.Direction = sign.ToSystem
	}
	if err := s.Sign(r, body); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) do(r *http.Request) (int, string) {
	f.t.Helper()
	resp, err := f.srv.Client().Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// signedCall sends a correctly signed call for job to /jobs/<job>.
func (f *fixture) signedCall(job, runID string, attempt int) (int, string) {
	f.t.Helper()
	body := callBody(f.t, job, runID, attempt)
	r := f.request("/jobs/"+job, body)
	f.sign(r, body, sign.Signer{})
	return f.do(r)
}

func TestSignedCallSucceedsAndLogsWithoutData(t *testing.T) {
	f := newFixture(t, nil)
	code, body := f.signedCall("send-reminders", "run-1", 1)
	if code != http.StatusOK || strings.TrimSpace(body) != "done" {
		t.Fatalf("got %d %q, want 200 done", code, body)
	}
	logs := f.logs.String()
	for _, want := range []string{`"job":"send-reminders"`, `"run_id":"run-1"`, `"attempt":1`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %s: %s", want, logs)
		}
	}
	if strings.Contains(logs, "personal-data-here") || strings.Contains(logs, `"data"`) {
		t.Fatalf("job data reached the log: %s", logs)
	}
}

func TestSignatureFailuresAnswer401(t *testing.T) {
	f := newFixture(t, nil)
	_, _, otherPriv := keyPair(t)
	body := callBody(t, "send-reminders", "run-1", 1)

	cases := map[string]func() *http.Request{
		"unsigned": func() *http.Request { return f.request("/jobs/send-reminders", body) },
		"bad signature (other key)": func() *http.Request {
			r := f.request("/jobs/send-reminders", body)
			f.sign(r, body, sign.Signer{Key: otherPriv})
			return r
		},
		"wrong direction": func() *http.Request {
			r := f.request("/jobs/send-reminders", body)
			f.sign(r, body, sign.Signer{Direction: sign.ToParadox})
			return r
		},
		"unknown key ID": func() *http.Request {
			r := f.request("/jobs/send-reminders", body)
			f.sign(r, body, sign.Signer{KeyID: "citadel"})
			return r
		},
		"changed body": func() *http.Request {
			changed := callBody(t, "send-reminders", "run-2", 1)
			r := f.request("/jobs/send-reminders", changed)
			f.sign(r, body, sign.Signer{})
			return r
		},
		"signed for another path": func() *http.Request {
			r := f.request("/jobs/send-reminders", body)
			r.URL.Path = "/jobs/other"
			f.sign(r, body, sign.Signer{})
			r.URL.Path = "/jobs/send-reminders"
			return r
		},
		"too old": func() *http.Request {
			r := f.request("/jobs/send-reminders", body)
			f.sign(r, body, sign.Signer{Now: func() time.Time { return time.Now().Add(-time.Hour) }})
			return r
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			if code, got := f.do(build()); code != http.StatusUnauthorized {
				t.Fatalf("got %d %q, want 401", code, got)
			}
		})
	}
	if strings.Contains(f.logs.String(), `"msg":"job call done"`) {
		t.Fatal("a refused call was treated as done")
	}
}

func TestSecondKeyIsAccepted(t *testing.T) {
	_, newPub, newPriv := keyPair(t)
	f := newFixture(t, func(c *Config) { c.ParadoxKeys = append(c.ParadoxKeys, newPub) })
	body := callBody(t, "send-reminders", "run-1", 1)
	r := f.request("/jobs/send-reminders", body)
	f.sign(r, body, sign.Signer{Key: newPriv})
	if code, got := f.do(r); code != http.StatusOK {
		t.Fatalf("got %d %q, want 200", code, got)
	}
	// The first key still works too.
	if code, got := f.signedCall("send-reminders", "run-2", 1); code != http.StatusOK {
		t.Fatalf("first key: got %d %q, want 200", code, got)
	}
}

func TestFailJobAnswers500EveryTime(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.FailJobs["broken"] = true })
	for attempt := 1; attempt <= 3; attempt++ {
		if code, got := f.signedCall("broken", "run-1", attempt); code != http.StatusInternalServerError {
			t.Fatalf("attempt %d: got %d %q, want 500", attempt, code, got)
		}
	}
	if code, _ := f.signedCall("fine", "run-2", 1); code != http.StatusOK {
		t.Fatalf("other job: got %d, want 200", code)
	}
}

func TestSlowJobWaitsThenSucceeds(t *testing.T) {
	const wait = 150 * time.Millisecond
	f := newFixture(t, func(c *Config) { c.SlowJobs["slow"] = wait })
	start := time.Now()
	code, got := f.signedCall("slow", "run-1", 1)
	if code != http.StatusOK {
		t.Fatalf("got %d %q, want 200", code, got)
	}
	if took := time.Since(start); took < wait {
		t.Fatalf("answered after %s, want at least %s", took, wait)
	}
}

func TestSlowFailJobWaitsThenFails(t *testing.T) {
	const wait = 50 * time.Millisecond
	f := newFixture(t, func(c *Config) {
		c.SlowJobs["slow"] = wait
		c.FailJobs["slow"] = true
	})
	start := time.Now()
	if code, _ := f.signedCall("slow", "run-1", 1); code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", code)
	}
	if took := time.Since(start); took < wait {
		t.Fatalf("answered after %s, want at least %s", took, wait)
	}
}

func TestSlowJobGivenUpByCallerIsNotDone(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SlowJobs["slow"] = time.Minute })
	body := callBody(t, "slow", "run-1", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := f.request("/jobs/slow", body).WithContext(ctx)
	f.sign(r, body, sign.Signer{})
	if _, err := f.srv.Client().Do(r); err == nil {
		t.Fatal("call answered before the slow job finished")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(f.logs.String(), "given up while waiting") {
		if time.Now().After(deadline) {
			t.Fatalf("handler did not stop waiting: %s", f.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(f.logs.String(), `"msg":"job call done"`) {
		t.Fatal("given-up call was recorded as done")
	}
}

func TestRepeatRunIDIsNotRedone(t *testing.T) {
	const wait = 200 * time.Millisecond
	f := newFixture(t, func(c *Config) { c.SlowJobs["slow"] = wait })
	if code, _ := f.signedCall("slow", "run-1", 1); code != http.StatusOK {
		t.Fatalf("first call: got %d", code)
	}
	start := time.Now()
	code, got := f.signedCall("slow", "run-1", 2)
	if code != http.StatusOK || strings.TrimSpace(got) != "already done" {
		t.Fatalf("repeat: got %d %q, want 200 already done", code, got)
	}
	if took := time.Since(start); took >= wait {
		t.Fatalf("repeat took %s: the work was redone", took)
	}
	if !strings.Contains(f.logs.String(), "repeat") {
		t.Fatalf("repeat not logged: %s", f.logs.String())
	}
}

func TestFailedRunIsTriedAgainNotTreatedAsRepeat(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.FailJobs["broken"] = true })
	f.signedCall("broken", "run-1", 1)
	if code, got := f.signedCall("broken", "run-1", 2); code != http.StatusInternalServerError {
		t.Fatalf("got %d %q, want 500 again", code, got)
	}
}

func TestInvalidCallsAnswer400(t *testing.T) {
	f := newFixture(t, nil)
	due := time.Date(2026, 11, 1, 1, 30, 0, 0, time.UTC)
	cases := map[string]struct {
		path string
		body string
	}{
		"job differs from path": {"/jobs/send-reminders", mustJSON(t, jobcall.Call{RunID: "r", Job: "other", Attempt: 1, DueAt: due})},
		"not JSON":              {"/jobs/send-reminders", "not json"},
		"empty body":            {"/jobs/send-reminders", ""},
		"missing run_id":        {"/jobs/send-reminders", mustJSON(t, jobcall.Call{Job: "send-reminders", Attempt: 1, DueAt: due})},
		"missing job":           {"/jobs/send-reminders", mustJSON(t, jobcall.Call{RunID: "r", Attempt: 1, DueAt: due})},
		"attempt zero":          {"/jobs/send-reminders", mustJSON(t, jobcall.Call{RunID: "r", Job: "send-reminders", DueAt: due})},
		"missing due_at":        {"/jobs/send-reminders", `{"run_id":"r","job":"send-reminders","attempt":1,"data":null}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := []byte(tc.body)
			r := f.request(tc.path, body)
			f.sign(r, body, sign.Signer{})
			if code, got := f.do(r); code != http.StatusBadRequest {
				t.Fatalf("got %d %q, want 400", code, got)
			}
		})
	}
}

func mustJSON(t *testing.T, c jobcall.Call) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOversizedBodyAnswers413(t *testing.T) {
	f := newFixture(t, nil)
	body := bytes.Repeat([]byte("x"), MaxBodyBytes+1)
	r := f.request("/jobs/send-reminders", body)
	f.sign(r, body, sign.Signer{})
	if code, got := f.do(r); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d %q, want 413", code, got)
	}
}

func TestBodyAtLimitIsRead(t *testing.T) {
	f := newFixture(t, nil)
	// Exactly the limit is read, then refused only because it is not a call.
	body := bytes.Repeat([]byte(" "), MaxBodyBytes)
	r := f.request("/jobs/send-reminders", body)
	f.sign(r, body, sign.Signer{})
	if code, got := f.do(r); code != http.StatusBadRequest {
		t.Fatalf("got %d %q, want 400", code, got)
	}
}

func TestRoutes(t *testing.T) {
	f := newFixture(t, nil)
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodHead, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/jobs/send-reminders", http.StatusMethodNotAllowed},
		{http.MethodPut, "/jobs/send-reminders", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodPost, "/jobs/", http.StatusNotFound},
		{http.MethodPost, "/jobs/a/b", http.StatusNotFound},
		{http.MethodPost, "/other", http.StatusNotFound},
	}
	for _, tc := range cases {
		r, err := http.NewRequest(tc.method, f.srv.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if code, _ := f.do(r); code != tc.want {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.path, code, tc.want)
		}
	}
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaultsAndValues(t *testing.T) {
	pubText, pub, _ := keyPair(t)
	nextText, next, _ := keyPair(t)

	c, err := LoadConfig(env(map[string]string{EnvPublicKey: pubText}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || len(c.ParadoxKeys) != 1 || !c.ParadoxKeys[0].Equal(pub) ||
		len(c.FailJobs) != 0 || len(c.SlowJobs) != 0 {
		t.Fatalf("defaults: %+v", c)
	}

	c, err = LoadConfig(env(map[string]string{
		EnvListen:    "127.0.0.1:9090",
		EnvPublicKey: pubText + " , " + nextText,
		EnvFailJobs:  " broken, also-broken ,",
		EnvSlowJobs:  "slow=30s, slower = 2m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9090" {
		t.Errorf("listen %q", c.Listen)
	}
	if len(c.ParadoxKeys) != 2 || !c.ParadoxKeys[1].Equal(next) {
		t.Errorf("keys: %d", len(c.ParadoxKeys))
	}
	if !c.FailJobs["broken"] || !c.FailJobs["also-broken"] || len(c.FailJobs) != 2 {
		t.Errorf("fail jobs %v", c.FailJobs)
	}
	if c.SlowJobs["slow"] != 30*time.Second || c.SlowJobs["slower"] != 2*time.Minute || len(c.SlowJobs) != 2 {
		t.Errorf("slow jobs %v", c.SlowJobs)
	}
}

func TestLoadConfigMissingKey(t *testing.T) {
	_, err := LoadConfig(env(nil))
	if err == nil || !strings.Contains(err.Error(), EnvPublicKey+" is required") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadConfigReportsEveryProblemAtOnce(t *testing.T) {
	pubText, _, _ := keyPair(t)
	_, privText, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfig(env(map[string]string{
		EnvListen:    "no-port",
		EnvPublicKey: pubText + "," + privText,
		EnvFailJobs:  "Bad_Name,dup,dup",
		EnvSlowJobs:  "noequals,slow=soon,neg=-1s,zero=0s,UPPER=1s,twice=1s,twice=2s",
	}))
	if err == nil {
		t.Fatal("bad settings accepted")
	}
	msg := err.Error()
	if strings.Contains(msg, privText) {
		t.Fatal("error message includes the private key")
	}
	wants := []string{
		EnvListen + " must be host:port",
		EnvPublicKey + " key 2: this is a private key",
		EnvFailJobs + ` job "Bad_Name"`,
		EnvFailJobs + ` job "dup" appears twice`,
		EnvSlowJobs + ` entry "noequals"`,
		EnvSlowJobs + ` job "slow" needs a positive duration`,
		EnvSlowJobs + ` job "neg" needs a positive duration`,
		EnvSlowJobs + ` job "zero" needs a positive duration`,
		EnvSlowJobs + ` job "UPPER" must be lower-case`,
		EnvSlowJobs + ` job "twice" appears twice`,
	}
	for _, w := range wants {
		if !strings.Contains(msg, w) {
			t.Errorf("error lacks %q:\n%s", w, msg)
		}
	}
	if n := strings.Count(msg, "\n") + 1; n != len(wants) {
		t.Errorf("got %d problems, want %d:\n%s", n, len(wants), msg)
	}
}

func TestLoadConfigKeyProblems(t *testing.T) {
	a, _, _ := keyPair(t)
	b, _, _ := keyPair(t)
	c, _, _ := keyPair(t)
	cases := map[string]string{
		"three keys":   a + "," + b + "," + c,
		"not a key":    "hello",
		"empty second": a + ",",
		"short key":    "ed25519-pub:AAAA",
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(env(map[string]string{EnvPublicKey: v})); err == nil ||
				!strings.Contains(err.Error(), EnvPublicKey) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestServeAnswersThenStopsCleanly(t *testing.T) {
	_, pub, _ := keyPair(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, Config{ParadoxKeys: []ed25519.PublicKey{pub}}, nil) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop")
	}
	if _, err := http.Get("http://" + ln.Addr().String() + "/healthz"); err == nil {
		t.Fatal("still answering after stop")
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	err = Serve(context.Background(), ln, Config{}, nil)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("got %v, want the listener's error", err)
	}
}
