package wake

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

const citadel = "citadel"

func TestWakeWithinReadyAtOnce(t *testing.T) {
	f := NewFake()
	if err := WakeWithin(context.Background(), f, citadel, time.Second); err != nil {
		t.Fatalf("WakeWithin: %v", err)
	}
	if n := len(f.WakeCalls()); n != 1 {
		t.Fatalf("wake calls = %d, want 1", n)
	}
}

func TestWakeWithinLateButInsideLimit(t *testing.T) {
	f := NewFake()
	f.ReadyAfter(citadel, 10*time.Millisecond)
	start := time.Now()
	if err := WakeWithin(context.Background(), f, citadel, time.Second); err != nil {
		t.Fatalf("WakeWithin: %v", err)
	}
	if d := time.Since(start); d < 10*time.Millisecond {
		t.Fatalf("returned after %s, before the system was ready", d)
	}
}

func TestWakeWithinLateBeyondLimitIsTimeout(t *testing.T) {
	f := NewFake()
	f.ReadyAfter(citadel, time.Second)
	err := WakeWithin(context.Background(), f, citadel, 10*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

// PX-RUN-01: a system that never gets ready gives ErrTimeout, named.
func TestWakeWithinNeverReadyIsTimeout(t *testing.T) {
	f := NewFake()
	f.NeverReady(citadel)
	start := time.Now()
	err := WakeWithin(context.Background(), f, citadel, 10*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !strings.Contains(err.Error(), citadel) {
		t.Fatalf("err %q does not name the system", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %s, the limit was not honoured", d)
	}
}

func TestWakeWithinFailingWake(t *testing.T) {
	boom := errors.New("sablier said no")
	f := NewFake()
	f.Fail(citadel, boom)
	err := WakeWithin(context.Background(), f, citadel, time.Second)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the waker's error", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatalf("a failed wake must not be reported as a timeout: %v", err)
	}
	if !strings.Contains(err.Error(), citadel) {
		t.Fatalf("err %q does not name the system", err)
	}
}

// A cancelled caller (for example a shutdown) is not a timeout.
func TestWakeWithinCallerCancelled(t *testing.T) {
	f := NewFake()
	f.NeverReady(citadel)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	err := WakeWithin(ctx, f, citadel, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatalf("a cancelled wake must not be reported as a timeout: %v", err)
	}
}

func TestWakeWithinCallerAlreadyCancelled(t *testing.T) {
	f := NewFake()
	f.ReadyAfter(citadel, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WakeWithin(ctx, f, citadel, time.Second)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want context.Canceled only", err)
	}
}

// The caller's own deadline running out first is the caller's, not ours.
func TestWakeWithinCallerDeadlineShorterThanLimit(t *testing.T) {
	f := NewFake()
	f.NeverReady(citadel)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err := WakeWithin(ctx, f, citadel, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want the caller's deadline, not ErrTimeout", err)
	}
}

func TestWakeWithinRefusesNonPositiveLimit(t *testing.T) {
	f := NewFake()
	for _, limit := range []time.Duration{0, -time.Second} {
		err := WakeWithin(context.Background(), f, citadel, limit)
		if err == nil || errors.Is(err, ErrTimeout) {
			t.Fatalf("limit %s: err = %v, want a refusal", limit, err)
		}
	}
	if n := len(f.WakeCalls()); n != 0 {
		t.Fatalf("wake calls = %d, want none for a refused limit", n)
	}
}

// syncBuffer lets the test read log output written by another goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// startLoop runs KeepAwakeLoop in a goroutine and returns a stop function
// that cancels it and waits for it to return.
func startLoop(t *testing.T, w Waker, every time.Duration, logger *slog.Logger) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		KeepAwakeLoop(ctx, w, citadel, every, logger)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("KeepAwakeLoop did not stop after cancel")
		}
	}
}

// waitForCalls waits until the fake has at least n keep-awake calls.
func waitForCalls(t *testing.T, f *Fake, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(f.KeepAwakeCalls()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d keep-awake calls, want %d", len(f.KeepAwakeCalls()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// PX-RUN-02: keep awake is renewed at the interval, not faster.
func TestKeepAwakeLoopCallsAtInterval(t *testing.T) {
	const every = 10 * time.Millisecond
	f := NewFake()
	logger, _ := testLogger()
	start := time.Now()
	stop := startLoop(t, f, every, logger)
	waitForCalls(t, f, 3)
	stop()

	calls := f.KeepAwakeCalls()
	// The first call waits one interval: the system was just woken.
	if gap := calls[0].At.Sub(start); gap < every/2 {
		t.Fatalf("first call after %s, want about %s", gap, every)
	}
	for i := 1; i < len(calls); i++ {
		if calls[i].System != citadel {
			t.Fatalf("call %d for %q, want %q", i, calls[i].System, citadel)
		}
		// Tickers do not fire early; half the interval allows for jitter.
		if gap := calls[i].At.Sub(calls[i-1].At); gap < every/2 {
			t.Fatalf("calls %d and %d only %s apart, want about %s", i-1, i, gap, every)
		}
	}
}

// A failed keep awake is logged and the loop carries on.
func TestKeepAwakeLoopSurvivesFailures(t *testing.T) {
	f := NewFake()
	f.FailKeepAwake(citadel, errors.New("sablier unreachable"))
	logger, logs := testLogger()
	stop := startLoop(t, f, 2*time.Millisecond, logger)
	waitForCalls(t, f, 3)
	// Recovery: once the waker answers again, calls go on.
	f.FailKeepAwake(citadel, nil)
	n := len(f.KeepAwakeCalls())
	waitForCalls(t, f, n+2)
	stop()

	out := logs.String()
	if !strings.Contains(out, "keep awake failed") || !strings.Contains(out, "sablier unreachable") ||
		!strings.Contains(out, "system="+citadel) {
		t.Fatalf("failure not logged with system and error:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("failure not logged as a warning:\n%s", out)
	}
}

func TestKeepAwakeLoopStopsOnCancel(t *testing.T) {
	f := NewFake()
	logger, _ := testLogger()
	stop := startLoop(t, f, 2*time.Millisecond, logger)
	waitForCalls(t, f, 1)
	stop()
	n := len(f.KeepAwakeCalls())
	time.Sleep(10 * time.Millisecond)
	if m := len(f.KeepAwakeCalls()); m != n {
		t.Fatalf("calls went on after cancel: %d, then %d", n, m)
	}
}

func TestKeepAwakeLoopAlreadyCancelledMakesNoCalls(t *testing.T) {
	f := NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	KeepAwakeLoop(ctx, f, citadel, time.Millisecond, nil) // nil logger is allowed
	if n := len(f.KeepAwakeCalls()); n != 0 {
		t.Fatalf("keep-awake calls = %d, want none", n)
	}
}

func TestKeepAwakeLoopRefusesNonPositiveInterval(t *testing.T) {
	f := NewFake()
	logger, logs := testLogger()
	done := make(chan struct{})
	go func() {
		defer close(done)
		KeepAwakeLoop(context.Background(), f, citadel, 0, logger)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("KeepAwakeLoop with a zero interval did not return")
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("zero interval not logged as an error:\n%s", logs.String())
	}
	if n := len(f.KeepAwakeCalls()); n != 0 {
		t.Fatalf("keep-awake calls = %d, want none", n)
	}
}
