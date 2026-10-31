package wake

import (
	"context"
	"sync"
	"time"
)

// Call is one recorded call to a Fake.
type Call struct {
	System string
	At     time.Time
}

// mode is how a Fake answers Wake for one system.
type mode int

const (
	readyAtOnce mode = iota // the zero value, so unknown systems are ready
	readyAfter
	neverReady
	failing
)

type behaviour struct {
	mode  mode
	delay time.Duration // readyAfter only
	err   error         // failing only
}

// Fake is the local stand-in Waker, used in tests and until the Sablier
// adapter is in place. Each system is ready at once unless set otherwise
// with ReadyAfter, NeverReady or Fail. It records every call. A Fake is safe
// for concurrent use, including changing a system's behaviour while calls
// are under way; make one with NewFake.
type Fake struct {
	mu        sync.Mutex
	wake      map[string]behaviour
	keepErr   map[string]error
	wakeCalls []Call
	keepCalls []Call
}

var _ Waker = (*Fake)(nil)

// NewFake returns a Fake on which every system is ready at once and
// KeepAwake always succeeds.
func NewFake() *Fake {
	return &Fake{
		wake:    make(map[string]behaviour),
		keepErr: make(map[string]error),
	}
}

// Ready makes Wake for the system return nil at once (the default).
func (f *Fake) Ready(system string) {
	f.set(system, behaviour{mode: readyAtOnce})
}

// ReadyAfter makes every Wake for the system return nil after delay, or
// ctx's error if ctx ends first.
func (f *Fake) ReadyAfter(system string, delay time.Duration) {
	f.set(system, behaviour{mode: readyAfter, delay: delay})
}

// NeverReady makes Wake for the system block until ctx ends, then return
// ctx's error.
func (f *Fake) NeverReady(system string) {
	f.set(system, behaviour{mode: neverReady})
}

// Fail makes Wake for the system return err at once.
func (f *Fake) Fail(system string, err error) {
	f.set(system, behaviour{mode: failing, err: err})
}

// FailKeepAwake makes KeepAwake for the system return err. A nil err makes
// it succeed again.
func (f *Fake) FailKeepAwake(system string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.keepErr, system)
		return
	}
	f.keepErr[system] = err
}

func (f *Fake) set(system string, b behaviour) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wake[system] = b
}

// Wake records the call, then answers as set for the system.
func (f *Fake) Wake(ctx context.Context, system string) error {
	f.mu.Lock()
	f.wakeCalls = append(f.wakeCalls, Call{System: system, At: time.Now()})
	b := f.wake[system]
	f.mu.Unlock()

	switch b.mode {
	case readyAfter:
		t := time.NewTimer(b.delay)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case neverReady:
		<-ctx.Done()
		return ctx.Err()
	case failing:
		return b.err
	default:
		return nil
	}
}

// KeepAwake records the call and returns the error set with FailKeepAwake,
// or nil.
func (f *Fake) KeepAwake(_ context.Context, system string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keepCalls = append(f.keepCalls, Call{System: system, At: time.Now()})
	return f.keepErr[system]
}

// WakeCalls returns a copy of every Wake call so far, oldest first.
func (f *Fake) WakeCalls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.wakeCalls...)
}

// KeepAwakeCalls returns a copy of every KeepAwake call so far, oldest
// first.
func (f *Fake) KeepAwakeCalls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.keepCalls...)
}
