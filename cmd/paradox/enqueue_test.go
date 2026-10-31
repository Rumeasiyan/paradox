package main

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Rumeasiyan/paradox/sign"
)

// fakeAPI checks every request's signature as PARADOX would and records it.
type fakeAPI struct {
	mu     sync.Mutex
	got    []*http.Request
	bodies []map[string]any
}

func newFakeAPI(t *testing.T, system string, pub ed25519.PublicKey, status int, answer string) (*httptest.Server, *fakeAPI) {
	f := &fakeAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, err := sign.Verifier{Direction: sign.ToParadox, Keys: func(id string) []ed25519.PublicKey {
			if id == system {
				return []ed25519.PublicKey{pub}
			}
			return nil
		}}.Verify(r, body)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"not_authenticated"}`)
			return
		}
		m := map[string]any{}
		json.Unmarshal(body, &m)
		f.mu.Lock()
		f.got = append(f.got, r)
		f.bodies = append(f.bodies, m)
		f.mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func keyFile(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pubText, privText, _ := sign.GenerateKey()
	pub, _ := sign.ParsePublicKey(pubText)
	path := filepath.Join(t.TempDir(), "citadel.key")
	os.WriteFile(path, []byte(privText+"\n"), 0o600)
	return path, pub
}

func TestEnqueueQueuesSigned(t *testing.T) {
	key, pub := keyFile(t)
	srv, f := newFakeAPI(t, "citadel", pub, 202, `{"run_id":"r1","state":"queued"}`)
	code, out, errOut := runArgs(t, noEnv, "enqueue", "-url", srv.URL+"/", "-system", "citadel", "-key", key,
		"-job", "generate-certificates", "-data", `{"course_id":42}`, "-run-at", "2026-11-01T22:00:00+05:30", "-request-id", "course-42")
	if code != 0 || !strings.Contains(out, `"run_id": "r1"`) {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	r, b := f.got[0], f.bodies[0]
	if r.Method != "POST" || r.URL.Path != "/api/jobs" || b["job"] != "generate-certificates" || b["request_id"] != "course-42" ||
		b["run_at"] != "2026-11-01T22:00:00+05:30" || b["data"].(map[string]any)["course_id"] != float64(42) {
		t.Fatalf("sent %s %s %v", r.Method, r.URL.Path, b)
	}
}

func TestEnqueueRandomRequestIDs(t *testing.T) {
	key, pub := keyFile(t)
	srv, f := newFakeAPI(t, "citadel", pub, 202, `{}`)
	for i := 0; i < 2; i++ {
		if code, _, errOut := runArgs(t, noEnv, "enqueue", "-url", srv.URL, "-system", "citadel", "-key", key, "-job", "ping"); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
	}
	a, b := f.bodies[0]["request_id"].(string), f.bodies[1]["request_id"].(string)
	if a == b || !strings.HasPrefix(a, "enqueue-") {
		t.Fatalf("request IDs %q %q", a, b)
	}
}

func TestEnqueueReadAndCancel(t *testing.T) {
	key, pub := keyFile(t)
	srv, f := newFakeAPI(t, "citadel", pub, 200, `{"state":"cancelled"}`)
	runArgs(t, noEnv, "enqueue", "-url", srv.URL, "-system", "citadel", "-key", key, "-read", "abc")
	runArgs(t, noEnv, "enqueue", "-url", srv.URL, "-system", "citadel", "-key", key, "-cancel", "abc")
	if len(f.got) != 2 || f.got[0].Method != "GET" || f.got[0].URL.Path != "/api/jobs/abc" ||
		f.got[1].Method != "POST" || f.got[1].URL.Path != "/api/jobs/abc/cancel" {
		t.Fatalf("requests %v", f.got)
	}
}

func TestEnqueueRefusalExitsOne(t *testing.T) {
	key, pub := keyFile(t)
	srv, _ := newFakeAPI(t, "citadel", pub, 403, `{"error":"not_allowed"}`)
	code, out, errOut := runArgs(t, noEnv, "enqueue", "-url", srv.URL, "-system", "citadel", "-key", key, "-job", "rebuild")
	if code != 1 || out != "" || !strings.Contains(errOut, "PARADOX answered 403") || !strings.Contains(errOut, "not_allowed") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	// Signed as a system the server does not know: 401.
	code, _, errOut = runArgs(t, noEnv, "enqueue", "-url", srv.URL, "-system", "website", "-key", key, "-job", "x")
	if code != 1 || !strings.Contains(errOut, "401") {
		t.Fatalf("unknown system: exit %d %s", code, errOut)
	}
}

func TestEnqueueUsageAndInputErrors(t *testing.T) {
	key, _ := keyFile(t)
	for _, args := range [][]string{
		{"enqueue"},
		{"enqueue", "-url", "http://x", "-system", "a", "-key", key},
		{"enqueue", "-url", "http://x", "-system", "a", "-key", key, "-job", "j", "-read", "r"},
	} {
		if code, _, _ := runArgs(t, noEnv, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	_, privText, _ := sign.GenerateKey()
	for name, args := range map[string][]string{
		"bad url":    {"-url", "ftp://x", "-system", "a", "-key", key, "-job", "j"},
		"bad data":   {"-url", "http://x", "-system", "a", "-key", key, "-job", "j", "-data", "{nope"},
		"bad run-at": {"-url", "http://x", "-system", "a", "-key", key, "-job", "j", "-run-at", "tonight"},
		"no key":     {"-url", "http://x", "-system", "a", "-key", filepath.Join(t.TempDir(), "missing"), "-job", "j"},
	} {
		code, _, errOut := runArgs(t, noEnv, append([]string{"enqueue"}, args...)...)
		if code != 1 || strings.Contains(errOut, privText) {
			t.Errorf("%s: exit %d %s", name, code, errOut)
		}
	}
}
