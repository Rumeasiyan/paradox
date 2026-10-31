package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rumeasiyan/paradox/sign"
)

// lockedBuffer is written by the server's logger while the test reads it.
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

func TestTestSystemRefusesToStartWithoutKey(t *testing.T) {
	code, _, errOut := runArgs(t, noEnv, "testsystem")
	if code != 1 || !strings.Contains(errOut, "TESTSYSTEM_PARADOX_PUBLIC_KEY is required") {
		t.Fatalf("exit %d, err %q", code, errOut)
	}
}

func TestTestSystemReportsEveryBadSetting(t *testing.T) {
	getenv := func(k string) string {
		return map[string]string{
			"TESTSYSTEM_LISTEN":    "no-port",
			"TESTSYSTEM_SLOW_JOBS": "slow=soon",
		}[k]
	}
	code, _, errOut := runArgs(t, getenv, "testsystem")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"refusing to start", "TESTSYSTEM_LISTEN", "TESTSYSTEM_PARADOX_PUBLIC_KEY", "TESTSYSTEM_SLOW_JOBS"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q: %s", want, errOut)
		}
	}
}

func TestTestSystemRejectsArguments(t *testing.T) {
	if code, _, _ := runArgs(t, noEnv, "testsystem", "extra"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestTestSystemServesUntilContextEnds(t *testing.T) {
	pub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		return map[string]string{
			"TESTSYSTEM_LISTEN":             "127.0.0.1:0",
			"TESTSYSTEM_PARADOX_PUBLIC_KEY": pub,
		}[k]
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &lockedBuffer{}
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- serveTestSystem(ctx, nil, logs, getenv, ready) }()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("stopped early: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("did not start")
	}
	resp, err := http.Get("http://" + addr + "/healthz")
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
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop")
	}
	if !strings.Contains(logs.String(), "test system stopped") {
		t.Fatalf("no clean stop logged: %s", logs.String())
	}
	if strings.Contains(logs.String(), pub) {
		t.Fatal("key text reached the log")
	}
}

func TestTestSystemListenFailure(t *testing.T) {
	pub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		return map[string]string{
			"TESTSYSTEM_LISTEN":             "256.0.0.1:1",
			"TESTSYSTEM_PARADOX_PUBLIC_KEY": pub,
		}[k]
	}
	if err := serveTestSystem(context.Background(), nil, &lockedBuffer{}, getenv, nil); err == nil {
		t.Fatal("bad address accepted")
	}
}
