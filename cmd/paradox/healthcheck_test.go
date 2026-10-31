package main

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Rumeasiyan/paradox/internal/health"
)

// healthServer starts a /healthz server on the loopback address and returns
// its port.
func healthServer(t *testing.T, check func(context.Context) error) string {
	t.Helper()
	srv := httptest.NewServer(health.Handler(check, nil))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func listenEnv(listen string) func(string) string {
	return func(name string) string {
		if name == "PARADOX_LISTEN" {
			return listen
		}
		return ""
	}
}

func TestRunHealthcheckHealthy(t *testing.T) {
	port := healthServer(t, func(context.Context) error { return nil })
	for _, listen := range []string{":" + port, "0.0.0.0:" + port, "127.0.0.1:" + port} {
		if err := runHealthcheck(listenEnv(listen)); err != nil {
			t.Errorf("PARADOX_LISTEN=%q: %v", listen, err)
		}
	}
}

func TestRunHealthcheckUnhealthy(t *testing.T) {
	port := healthServer(t, func(context.Context) error { return errors.New("database down") })
	err := runHealthcheck(listenEnv(":" + port))
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want one naming 503", err)
	}
}

func TestRunHealthcheckNothingListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	err = runHealthcheck(listenEnv(":" + port))
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("err = %v, want not reachable", err)
	}
}

func TestRunHealthcheckBadListen(t *testing.T) {
	err := runHealthcheck(listenEnv("not-an-address"))
	if err == nil || !strings.Contains(err.Error(), "PARADOX_LISTEN") {
		t.Fatalf("err = %v, want one naming PARADOX_LISTEN", err)
	}
}

func TestHealthcheckFlagExitCodes(t *testing.T) {
	healthy := healthServer(t, func(context.Context) error { return nil })
	if code, _, stderr := runArgs(t, listenEnv(":"+healthy), "-healthcheck"); code != 0 {
		t.Fatalf("healthy: exit %d, stderr %q", code, stderr)
	}
	sick := healthServer(t, func(context.Context) error { return errors.New("database down") })
	code, _, stderr := runArgs(t, listenEnv(":"+sick), "-healthcheck")
	if code != 1 || !strings.Contains(stderr, "paradox:") {
		t.Fatalf("unhealthy: exit %d, stderr %q", code, stderr)
	}
	if strings.Contains(stderr, "database down") {
		t.Fatalf("the service's failure reason reached the caller: %q", stderr)
	}
}
