package testsystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Rumeasiyan/paradox/internal/jobcall"
	"github.com/Rumeasiyan/paradox/sign"
)

// MaxBodyBytes is the largest job call body accepted; larger ones get 413.
const MaxBodyBytes = 1 << 20

// ShutdownGrace is how long Serve lets calls in progress finish once its
// context ends. Slow jobs still waiting after that are cut off.
const ShutdownGrace = 10 * time.Second

type handler struct {
	cfg    Config
	log    *slog.Logger
	mu     sync.Mutex
	done   map[string]bool // run_ids that already succeeded
	routes *http.ServeMux
}

// New returns the test system's HTTP handler. A nil logger discards logs.
func New(cfg Config, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	h := &handler{cfg: cfg, log: logger, done: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs/{name}", h.job)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, "ok")
	})
	h.routes = mux
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.routes.ServeHTTP(w, r) }

func (h *handler) job(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.log.Warn("job call refused: body too large", "job", name, "limit_bytes", MaxBodyBytes)
			answer(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		h.log.Warn("job call refused: body not read", "job", name, "error", err)
		answer(w, http.StatusBadRequest, "body not read")
		return
	}

	// The signature is checked before anything else is done with the call
	// (contract section 5, PX-SIG-02).
	if err := sign.VerifyFromParadox(r, body, h.cfg.ParadoxKeys...); err != nil {
		h.log.Warn("job call refused: signature", "job", name, "error", err)
		answer(w, http.StatusUnauthorized, "signature refused")
		return
	}

	var call jobcall.Call
	if err := json.Unmarshal(body, &call); err != nil {
		h.log.Warn("job call refused: body is not a job call", "job", name)
		answer(w, http.StatusBadRequest, "body is not a job call")
		return
	}
	if msg := invalid(call, name); msg != "" {
		h.log.Warn("job call refused: "+msg, "job", name, "run_id", call.RunID, "attempt", call.Attempt)
		answer(w, http.StatusBadRequest, msg)
		return
	}
	attrs := []any{"job", call.Job, "run_id", call.RunID, "attempt", call.Attempt}

	if h.succeeded(call.RunID) {
		h.log.Info("job call is a repeat of a run already done; not redoing it", attrs...)
		answer(w, http.StatusOK, "already done")
		return
	}

	if d, ok := h.cfg.SlowJobs[call.Job]; ok {
		h.log.Info("job call is slow on purpose", append(attrs, "wait", d.String())...)
		if err := sleepCtx(r.Context(), d); err != nil {
			h.log.Warn("job call given up while waiting", append(attrs, "error", err)...)
			return
		}
	}

	if h.cfg.FailJobs[call.Job] {
		h.log.Info("job call failed on purpose", attrs...)
		answer(w, http.StatusInternalServerError, "failed on purpose")
		return
	}

	h.mu.Lock()
	h.done[call.RunID] = true
	h.mu.Unlock()
	h.log.Info("job call done", attrs...)
	answer(w, http.StatusOK, "done")
}

func (h *handler) succeeded(runID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.done[runID]
}

// invalid returns why a call cannot be accepted, or "" if it can.
func invalid(c jobcall.Call, path string) string {
	switch {
	case c.RunID == "":
		return "run_id is missing"
	case c.Job == "":
		return "job is missing"
	case c.Job != path:
		return fmt.Sprintf("job %q does not match the path", c.Job)
	case c.Attempt < 1:
		return "attempt must be 1 or more"
	case c.DueAt.IsZero():
		return "due_at is missing"
	}
	return ""
}

// answer writes a short plain-text answer. It never echoes the call's data.
func answer(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintln(w, msg)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Serve answers calls on ln until ctx ends, then shuts down: calls in
// progress get ShutdownGrace to finish. It returns nil after a clean stop.
func Serve(ctx context.Context, ln net.Listener, cfg Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	srv := &http.Server{
		Handler:           New(cfg, logger),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	logger.Info("test system listening",
		"address", ln.Addr().String(),
		"paradox_keys", len(cfg.ParadoxKeys),
		"fail_jobs", names(cfg.FailJobs),
		"slow_jobs", names(cfg.SlowJobs))

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	logger.Info("test system stopping")
	shutCtx, cancel := context.WithTimeout(context.Background(), ShutdownGrace)
	defer cancel()
	err := srv.Shutdown(shutCtx)
	if err != nil {
		srv.Close()
	}
	if serveErr := <-served; !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("test system stopped")
	return nil
}
