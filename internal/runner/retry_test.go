package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rumeasiyan/paradox/internal/report"
	"github.com/Rumeasiyan/paradox/internal/schedule"
)

type fakeReporter struct {
	mu       sync.Mutex
	failures []report.Failure
}

func (f *fakeReporter) Heartbeat(context.Context) error { return nil }

func (f *fakeReporter) Failure(_ context.Context, x report.Failure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, x)
	return nil
}

func (f *fakeReporter) list() []report.Failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]report.Failure(nil), f.failures...)
}

func TestRetryDelaysFollowTheSettingAndRepeatTheLast(t *testing.T) {
	d := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	for made, want := range map[int]time.Duration{1: time.Minute, 2: 5 * time.Minute, 3: 15 * time.Minute, 4: 15 * time.Minute, 9: 15 * time.Minute, 0: time.Minute} {
		if got := retryDelay(d, made); got != want {
			t.Errorf("after %d attempts: %v, want %v", made, got, want)
		}
	}
	if got := retryDelay(nil, 1); got != time.Minute {
		t.Errorf("no delays set: %v", got)
	}
}

// A job that keeps failing is retried until its attempts run out, then
// reported once (PX-REP-01, PX-REP-02; how it works, Flow A steps 13 to 15).
func TestFailingJobIsRetriedThenReportedOnce(t *testing.T) {
	rep := &fakeReporter{}
	h := newHarness(t, []string{"fail"}, nil, func(d *Deps) {
		d.RetryDelays = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}
		d.Reporter = rep
	}, jobSpec{"citadel", schedule.WakeSablier, "fail", 10, 2})
	id := h.due("fail")
	r := h.wait(id, 20*time.Second)
	if r.state != "failed" || r.attempts != 3 || !r.finished || r.hasData {
		t.Fatalf("run %+v", r)
	}
	if got := h.attempts(id); strings.Join(got, ",") != "error:500,error:500,error:500" {
		t.Fatalf("attempts %v", got)
	}
	var reported bool
	h.pool.QueryRow(context.Background(), `SELECT reported_at IS NOT NULL FROM run WHERE id = $1`, id).Scan(&reported)
	fs := rep.list()
	if !reported || len(fs) != 1 {
		t.Fatalf("reported_at set %v, %d reports; want one", reported, len(fs))
	}
	f := fs[0]
	if f.System != "citadel" || f.Job != "fail" || f.RunID != id || f.Attempts != 3 || !strings.Contains(f.LastError, "answered 500") {
		t.Fatalf("report %+v", f)
	}
	if n := len(h.waker.WakeCalls()); n != 3 {
		t.Fatalf("woken %d times, want once per attempt", n)
	}
}

func TestSucceedingRunIsNeverReported(t *testing.T) {
	rep := &fakeReporter{}
	h := newHarness(t, nil, nil, func(d *Deps) { d.Reporter = rep }, jobSpec{"citadel", schedule.WakeNone, "ok", 10, 2})
	if r := h.wait(h.due("ok"), 10*time.Second); r.state != "succeeded" || len(rep.list()) != 0 {
		t.Fatalf("run %+v, reports %d", r, len(rep.list()))
	}
}
