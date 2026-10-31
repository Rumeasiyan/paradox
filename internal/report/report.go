// Package report sends PARADOX's two reports to the push monitor: the
// heartbeat (PX-REP-03) and the report for a run that failed every attempt
// (PX-REP-02). This is link 10 in PRD Annex B. Log is the local stand-in used
// when PARADOX_HEARTBEAT_URL or PARADOX_FAILURE_URL is empty; the push-monitor
// adapter implements the same Reporter interface.
package report

import (
	"context"
	"log/slog"
	"unicode/utf8"
)

// MaxErrorBytes is the most bytes of a run's last error that a Failure
// carries. Longer errors are cut so a report stays small.
const MaxErrorBytes = 2048

// Failure describes a run that failed every attempt (PX-REP-02). It holds
// only names, a count and the last error, never job data: payloads may carry
// personal data and must not leave PARADOX in a report (PX-NFR-06).
type Failure struct {
	System    string // system name, for example citadel
	Job       string // job name within the system
	RunID     string // id of the run record
	Attempts  int    // attempts made, including the first
	LastError string // last attempt's error, at most MaxErrorBytes
}

// NewFailure builds a Failure, cutting lastError to MaxErrorBytes.
func NewFailure(system, job, runID string, attempts int, lastError string) Failure {
	return Failure{
		System:    system,
		Job:       job,
		RunID:     runID,
		Attempts:  attempts,
		LastError: TruncateError(lastError),
	}
}

// Truncated returns a copy of f with LastError cut to MaxErrorBytes.
// Reporters call it so a Failure built by hand is bounded too.
func (f Failure) Truncated() Failure {
	f.LastError = TruncateError(f.LastError)
	return f
}

// TruncateError cuts s to at most MaxErrorBytes bytes. It never splits a
// UTF-8 character: if the limit falls inside one, the whole character is
// dropped.
func TruncateError(s string) string {
	if len(s) <= MaxErrorBytes {
		return s
	}
	cut := MaxErrorBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Reporter sends reports to the monitor. Implementations must be safe for
// use from several goroutines.
type Reporter interface {
	// Heartbeat tells the monitor PARADOX is alive (PX-REP-03).
	Heartbeat(ctx context.Context) error
	// Failure reports a run that failed every attempt (PX-REP-02).
	Failure(ctx context.Context, f Failure) error
}

// Log is the Reporter used when no push URL is set: it writes reports to the
// log instead. A nil Logger means slog.Default().
type Log struct {
	Logger *slog.Logger
}

var _ Reporter = Log{}

func (l Log) logger() *slog.Logger {
	if l.Logger == nil {
		return slog.Default()
	}
	return l.Logger
}

// Heartbeat logs at Debug, so a heartbeat every minute does not flood the
// log at the usual Info level.
func (l Log) Heartbeat(ctx context.Context) error {
	l.logger().DebugContext(ctx, "heartbeat")
	return nil
}

// Failure logs the report at Error with each field as an attribute.
func (l Log) Failure(ctx context.Context, f Failure) error {
	f = f.Truncated()
	l.logger().ErrorContext(ctx, "run failed every attempt",
		slog.String("system", f.System),
		slog.String("job", f.Job),
		slog.String("run_id", f.RunID),
		slog.Int("attempts", f.Attempts),
		slog.String("last_error", f.LastError),
	)
	return nil
}
