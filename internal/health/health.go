// Package health answers PARADOX's health check (PX-REP-04).
//
// The container platform runs `paradox -healthcheck` inside the container.
// That command probes the running service's /healthz over the loopback
// address, and the service answers healthy only when its database answers
// too. A service that is up but cut off from its database cannot run or
// record jobs, so it must not count as healthy.
package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Path is where the service answers the health check.
const Path = "/healthz"

// DBTimeout is the longest the database ping may take.
const DBTimeout = 2 * time.Second

// ProbeTimeout is the longest the health check command waits for an answer.
// It is longer than DBTimeout so a slow database shows as 503, not a timeout.
const ProbeTimeout = 3 * time.Second

// Handler answers GET /healthz with 200 "ok" when check passes and 503
// "unhealthy" when it fails. The reason is logged, never sent: the caller
// may be anyone who can reach the port, and a database error can name hosts
// and users.
func Handler(check func(context.Context) error, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(Path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := check(r.Context()); err != nil {
			logger.Warn("health check failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "unhealthy\n")
			return
		}
		io.WriteString(w, "ok\n")
	})
	return mux
}

// DBCheck returns a check that pings the database with a DBTimeout limit.
func DBCheck(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		if pool == nil {
			return errors.New("database: no connection pool")
		}
		ctx, cancel := context.WithTimeout(ctx, DBTimeout)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			return fmt.Errorf("database not reachable: %w", err)
		}
		return nil
	}
}

// Probe asks url for its health with a ProbeTimeout limit. Only a 200 answer
// counts as healthy.
func Probe(ctx context.Context, url string) error {
	return probe(ctx, url, ProbeTimeout)
}

func probe(ctx context.Context, url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("health check address %q is not valid: %w", url, err)
	}
	// A client of its own: no proxy from the environment and no redirects,
	// so the probe only ever talks to the service it was pointed at.
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("service did not answer %s within %s", url, timeout)
		}
		return fmt.Errorf("service not reachable at %s: %w", url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("service at %s answered %s", url, resp.Status)
	}
	return nil
}

// LoopbackURL turns a listen address (PARADOX_LISTEN, such as ":8080",
// "0.0.0.0:8080", "[::]:8080" or "127.0.0.1:9090") into the health check
// address on 127.0.0.1. The probe runs inside the container, so whatever
// address the service binds, the loopback address reaches it; only the port
// is taken from the setting.
func LoopbackURL(listen string) (string, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("PARADOX_LISTEN %q is not host:port: %w", listen, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("PARADOX_LISTEN %q has no port from 1 to 65535", listen)
	}
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(n)) + Path, nil
}
