package wake

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestFakeDefaultsToReady(t *testing.T) {
	f := NewFake()
	if err := f.Wake(context.Background(), "unknown"); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if err := f.KeepAwake(context.Background(), "unknown"); err != nil {
		t.Fatalf("KeepAwake: %v", err)
	}
}

func TestFakeBehaviourIsPerSystem(t *testing.T) {
	boom := errors.New("boom")
	f := NewFake()
	f.Fail("other", boom)
	if err := f.Wake(context.Background(), citadel); err != nil {
		t.Fatalf("citadel: %v, want ready", err)
	}
	if err := f.Wake(context.Background(), "other"); !errors.Is(err, boom) {
		t.Fatalf("other: %v, want boom", err)
	}
	// Ready puts a system back to the default.
	f.Ready("other")
	if err := f.Wake(context.Background(), "other"); err != nil {
		t.Fatalf("other after Ready: %v", err)
	}
}

func TestFakeNeverReadyBlocksUntilContextEnds(t *testing.T) {
	f := NewFake()
	f.NeverReady(citadel)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := f.Wake(ctx, citadel); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestFakeReadyAfterStopsOnContextEnd(t *testing.T) {
	f := NewFake()
	f.ReadyAfter(citadel, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.Wake(ctx, citadel); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestFakeKeepAwakeFailure(t *testing.T) {
	boom := errors.New("boom")
	f := NewFake()
	f.FailKeepAwake(citadel, boom)
	if err := f.KeepAwake(context.Background(), citadel); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if err := f.KeepAwake(context.Background(), "other"); err != nil {
		t.Fatalf("other: %v, want nil", err)
	}
	f.FailKeepAwake(citadel, nil)
	if err := f.KeepAwake(context.Background(), citadel); err != nil {
		t.Fatalf("after clearing: %v, want nil", err)
	}
}

func TestFakeRecordsCallsInOrder(t *testing.T) {
	f := NewFake()
	f.Fail("other", errors.New("boom"))
	before := time.Now()
	_ = f.Wake(context.Background(), citadel)
	_ = f.Wake(context.Background(), "other") // failed calls are recorded too
	_ = f.KeepAwake(context.Background(), citadel)

	wakes := f.WakeCalls()
	if len(wakes) != 2 || wakes[0].System != citadel || wakes[1].System != "other" {
		t.Fatalf("wake calls = %+v", wakes)
	}
	if wakes[0].At.Before(before) || wakes[1].At.Before(wakes[0].At) {
		t.Fatalf("wake call times out of order: %+v", wakes)
	}
	keeps := f.KeepAwakeCalls()
	if len(keeps) != 1 || keeps[0].System != citadel || keeps[0].At.Before(before) {
		t.Fatalf("keep-awake calls = %+v", keeps)
	}

	// The accessors return copies.
	wakes[0].System = "changed"
	if f.WakeCalls()[0].System != citadel {
		t.Fatal("WakeCalls exposed the fake's own slice")
	}
}

// Run with -race: calls, setters and accessors from many goroutines.
func TestFakeConcurrentUse(t *testing.T) {
	f := NewFake()
	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			system := fmt.Sprintf("system-%d", i%5)
			_ = f.Wake(context.Background(), system)
			_ = f.KeepAwake(context.Background(), system)
			f.FailKeepAwake(system, nil)
			f.Ready(system)
			_ = f.WakeCalls()
			_ = f.KeepAwakeCalls()
		}()
	}
	wg.Wait()
	if w, k := len(f.WakeCalls()), len(f.KeepAwakeCalls()); w != n || k != n {
		t.Fatalf("recorded %d wakes and %d keep-awakes, want %d each", w, k, n)
	}
}
