package report

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReporter counts heartbeats and can fail or block them.
type fakeReporter struct {
	mu      sync.Mutex
	calls   int
	err     error
	block   bool // wait for the heartbeat context to end
	ctxErrs []error
	beats   chan struct{}
}

func newFake() *fakeReporter { return &fakeReporter{beats: make(chan struct{}, 100)} }

func (f *fakeReporter) Heartbeat(ctx context.Context) error {
	f.mu.Lock()
	block, err := f.block, f.err
	f.mu.Unlock()
	if block {
		<-ctx.Done()
	}
	f.mu.Lock()
	f.calls++
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	f.mu.Unlock()
	select {
	case f.beats <- struct{}{}:
	default:
	}
	return err
}

func (f *fakeReporter) Failure(context.Context, Failure) error { return nil }

func (f *fakeReporter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// waitBeats waits for n heartbeats or fails the test.
func waitBeats(t *testing.T, f *fakeReporter, n int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for i := 0; i < n; i++ {
		select {
		case <-f.beats:
		case <-deadline:
			t.Fatalf("got %d heartbeats, want %d within %s", i, n, within)
		}
	}
}

// startLoop runs HeartbeatLoop and returns a stop function that cancels it
// and waits for it to return.
func startLoop(t *testing.T, r Reporter, every time.Duration, logger *slog.Logger) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		HeartbeatLoop(ctx, r, every, logger)
		close(done)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("HeartbeatLoop did not return after cancel")
		}
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return stop
}

func TestHeartbeatLoopSendsAtOnce(t *testing.T) {
	f := newFake()
	logger, _ := newLogger(slog.LevelDebug)
	stop := startLoop(t, f, time.Hour, logger)
	waitBeats(t, f, 1, time.Second)
	stop()
	if got := f.count(); got != 1 {
		t.Errorf("got %d heartbeats with an hour interval, want 1", got)
	}
}

func TestHeartbeatLoopSendsAtInterval(t *testing.T) {
	f := newFake()
	logger, _ := newLogger(slog.LevelDebug)
	start := time.Now()
	stop := startLoop(t, f, 10*time.Millisecond, logger)
	waitBeats(t, f, 4, 2*time.Second)
	stop()
	// One at once plus three ticks: at least three intervals must pass.
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("4 heartbeats in %s, faster than the interval allows", elapsed)
	}
}

func TestHeartbeatLoopCarriesOnAfterErrors(t *testing.T) {
	f := newFake()
	f.err = errors.New("monitor unreachable")
	logger, buf := newLogger(slog.LevelDebug)
	stop := startLoop(t, f, 5*time.Millisecond, logger)
	waitBeats(t, f, 3, 2*time.Second)
	stop()
	recs := buf.records(t)
	warns := 0
	for _, r := range recs {
		if r["level"] == "WARN" && r["msg"] == "heartbeat failed" {
			warns++
			if r["error"] != "monitor unreachable" {
				t.Errorf("warn error = %v", r["error"])
			}
		}
	}
	if warns < 3 {
		t.Errorf("got %d warnings, want at least 3: %s", warns, buf.String())
	}
}

func TestHeartbeatLoopStopsOnCancel(t *testing.T) {
	f := newFake()
	logger, _ := newLogger(slog.LevelDebug)
	stop := startLoop(t, f, 5*time.Millisecond, logger)
	waitBeats(t, f, 2, 2*time.Second)
	stop()
	after := f.count()
	time.Sleep(30 * time.Millisecond)
	if got := f.count(); got != after {
		t.Errorf("heartbeats went on after cancel: %d then %d", after, got)
	}
}

func TestHeartbeatLoopCancelledBeforeStart(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	HeartbeatLoop(ctx, f, time.Millisecond, nil)
	if got := f.count(); got != 0 {
		t.Errorf("got %d heartbeats on a cancelled context, want 0", got)
	}
}

func TestHeartbeatLoopTimesOutHungHeartbeat(t *testing.T) {
	f := newFake()
	f.block = true
	logger, buf := newLogger(slog.LevelDebug)
	stop := startLoop(t, f, 10*time.Millisecond, logger)
	waitBeats(t, f, 2, 2*time.Second)
	stop()
	f.mu.Lock()
	first := f.ctxErrs[0]
	f.mu.Unlock()
	if !errors.Is(first, context.DeadlineExceeded) {
		t.Errorf("hung heartbeat context ended with %v, want deadline exceeded", first)
	}
	// The blocking fake returns nil, so no warning is due.
	if strings.Contains(buf.String(), "heartbeat failed") {
		t.Errorf("unexpected warning: %s", buf.String())
	}
}

func TestHeartbeatLoopWithLogReporter(t *testing.T) {
	logger, buf := newLogger(slog.LevelDebug)
	stop := startLoop(t, Log{Logger: logger}, time.Hour, logger)
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(buf.String(), `"msg":"heartbeat"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no heartbeat logged: %q", buf.String())
		}
		time.Sleep(time.Millisecond)
	}
	stop()
}

func TestHeartbeatLoopZeroIntervalUsesDefault(t *testing.T) {
	// A zero interval would make time.NewTicker panic; the loop must use the
	// default instead and still send the first heartbeat at once.
	f := newFake()
	stop := startLoop(t, f, 0, nil)
	waitBeats(t, f, 1, time.Second)
	stop()
	if got := f.count(); got != 1 {
		t.Errorf("got %d heartbeats, want 1", got)
	}
}
