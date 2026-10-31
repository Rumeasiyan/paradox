// Package wake starts sleeping systems before PARADOX calls them and keeps
// them awake while a job runs (PX-RUN-01, PX-RUN-02; PRD Annex B link 7).
//
// The runner talks only to the Waker interface. Sablier is the real
// adapter; Fake is the local stand-in for tests.
package wake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Waker starts a system and keeps it running.
type Waker interface {
	// Wake asks for the system to be started and blocks until it is ready
	// to take calls, or ctx ends. It returns nil only when the system is
	// ready.
	Wake(ctx context.Context, system string) error

	// KeepAwake tells the waker the system is still in use, so it is not
	// stopped while a job runs.
	KeepAwake(ctx context.Context, system string) error
}

// ErrTimeout means a system was not ready within the wake limit. The runner
// counts it as a failed attempt (PX-RUN-01). Test for it with errors.Is.
var ErrTimeout = errors.New("system not ready in time")

// WakeWithin wakes the system and waits at most limit for it to be ready
// (PX-RUN-01; the limit is PARADOX_WAKE_TIMEOUT, default 2 minutes).
//
// It returns nil when the system is ready, an error wrapping ErrTimeout when
// limit runs out first, ctx's own error (wrapped) when the caller's context
// ends first, and the waker's error (wrapped) when the wake itself fails.
// Every error names the system. A limit of zero or less is refused.
func WakeWithin(ctx context.Context, w Waker, system string, limit time.Duration) error {
	if limit <= 0 {
		return fmt.Errorf("wake %s: limit must be positive, got %s", system, limit)
	}
	wctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	err := w.Wake(wctx, system)
	if err == nil {
		return nil
	}
	// The caller's context ending is not a timeout of this wake: report it
	// as it is, so a shutdown is not mistaken for a slow system.
	if ctx.Err() != nil {
		return fmt.Errorf("wake %s: %w", system, ctx.Err())
	}
	if errors.Is(wctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("wake %s: not ready within %s: %w", system, limit, ErrTimeout)
	}
	return fmt.Errorf("wake %s: %w", system, err)
}

// KeepAwakeLoop calls w.KeepAwake for the system every `every` until ctx
// ends (PX-RUN-02; `every` is PARADOX_KEEP_AWAKE_EVERY, default 60 seconds).
// Run it in its own goroutine for as long as the job runs, and cancel ctx
// when the job ends.
//
// The first call is made one interval after the start, since the system was
// just woken. A failed call is logged as a warning and the loop carries on:
// keeping awake never stops a job. A nil logger means slog.Default(). An
// interval of zero or less is logged as an error and the loop returns at
// once.
func KeepAwakeLoop(ctx context.Context, w Waker, system string, every time.Duration, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if every <= 0 {
		logger.Error("keep awake not started: interval must be positive",
			"system", system, "every", every)
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := w.KeepAwake(ctx, system); err != nil {
			if ctx.Err() != nil {
				// The job ended during the call; nothing to report.
				return
			}
			logger.Warn("keep awake failed", "system", system, "error", err)
		}
	}
}
