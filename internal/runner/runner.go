// Package runner turns due jobs into runs and carries each run out: wake
// the system, keep it awake, call its job address with a signed request,
// and record every attempt (PX-RUN-01 to PX-RUN-03, PX-REC-01; how it works,
// Flow A steps 4 to 15).
//
// The database pool has only two connections (internal/db), and a wake or
// a call can take minutes. So no transaction is ever held open across a
// wake or a call: each run is a short transaction before, the slow work
// with no connection held, and a short transaction after.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/Rumeasiyan/paradox/internal/jobcall"
	"github.com/Rumeasiyan/paradox/internal/report"
	"github.com/Rumeasiyan/paradox/internal/schedule"
	"github.com/Rumeasiyan/paradox/internal/wake"
	"github.com/Rumeasiyan/paradox/sign"
)

// maxErrorBytes is how much of a failure is recorded (PX-REC-01).
const maxErrorBytes = 2048

// RunArgs is the River job that carries one run. It holds only the run's ID
// and time limit; the job data stays on the run row, where it is wiped
// when the run ends.
type RunArgs struct {
	RunID    string `json:"run_id"`
	TimeoutS int    `json:"timeout_s"`
}

// Kind names the River job.
func (RunArgs) Kind() string { return "paradox_run" }

// Deps is everything a run needs from outside.
type Deps struct {
	Pool           *pgxpool.Pool
	Waker          wake.Waker
	Key            sign.Signer // KeyID sign.ParadoxKeyID, Direction sign.ToSystem
	HTTP           *http.Client
	Logger         *slog.Logger
	Location       *time.Location  // PARADOX_TIMEZONE
	WakeTimeout    time.Duration   // PARADOX_WAKE_TIMEOUT
	KeepAwakeEvery time.Duration   // PARADOX_KEEP_AWAKE_EVERY
	SystemPort     int             // PARADOX_SYSTEM_PORT
	RetryDelays    []time.Duration // PARADOX_RETRY_DELAYS; the last one repeats
	Reporter       report.Reporter
	// BaseURL returns where a system is called. Nil means its address, or
	// http://<name>:<SystemPort> when it has none. Tests replace it.
	BaseURL func(system, address string) string
	Now     func() time.Time
}

func (d *Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Deps) baseURL(system, address string) string {
	if d.BaseURL != nil {
		return d.BaseURL(system, address)
	}
	if address != "" {
		return address
	}
	return "http://" + system + ":" + strconv.Itoa(d.SystemPort)
}

// Worker is the River worker that carries out runs.
type Worker struct {
	river.WorkerDefaults[RunArgs]
	D *Deps
}

// NextRetry spaces retries by PARADOX_RETRY_DELAYS (PX-REP-01): the first
// retry waits the first delay, the second the second, and any further
// retry the last one. job.Attempt counts attempts made so far.
func (w *Worker) NextRetry(job *river.Job[RunArgs]) time.Time {
	return w.D.now().Add(retryDelay(w.D.RetryDelays, job.Attempt))
}

func retryDelay(delays []time.Duration, attemptsMade int) time.Duration {
	if len(delays) == 0 {
		return time.Minute
	}
	i := attemptsMade - 1
	if i < 0 {
		i = 0
	}
	if i >= len(delays) {
		i = len(delays) - 1
	}
	return delays[i]
}

// Timeout lets an attempt last as long as a wake plus the job's own time
// limit, with a margin for recording the result. River's own default (one
// minute) would cut longer jobs off.
func (w *Worker) Timeout(job *river.Job[RunArgs]) time.Duration {
	return w.D.WakeTimeout + time.Duration(job.Args.TimeoutS)*time.Second + 30*time.Second
}

// run is what an attempt needs to know about its run.
type run struct {
	id          string
	job         string
	system      string
	wakeMode    string
	address     string
	data        []byte
	dueAt       time.Time
	attempt     int
	maxAttempts int
	timeout     time.Duration
	attemptID   int64
}

// outcome of one attempt.
type outcome struct {
	result string // success, error, timeout, wake_timeout
	status int    // the system's HTTP status, 0 if none
	err    string // empty on success
}

// Work carries out one attempt of a run.
func (w *Worker) Work(ctx context.Context, job *river.Job[RunArgs]) error {
	r, ok, err := w.start(ctx, job.Args.RunID)
	if err != nil {
		return err
	}
	if !ok {
		return nil // cancelled or already finished: nothing to do
	}
	log := w.D.Logger.With("system", r.system, "job", r.job, "run_id", r.id, "attempt", r.attempt)

	out := w.attempt(ctx, r, log)
	final, err := w.finish(context.WithoutCancel(ctx), r, out)
	if err != nil {
		return err
	}
	switch {
	case out.result == "success":
		log.Info("run succeeded", "status", out.status)
		return nil
	case final:
		log.Warn("run failed, no attempts left", "outcome", out.result, "error", out.err)
		w.reportFailure(context.WithoutCancel(ctx), r, out, log)
		return river.JobCancel(errors.New(out.err))
	default:
		log.Info("attempt failed, will retry", "outcome", out.result, "error", out.err)
		return errors.New(out.err)
	}
}

// start marks the run running and opens an attempt, in one short
// transaction. ok is false when the run is no longer queued.
func (w *Worker) start(ctx context.Context, runID string) (r run, ok bool, err error) {
	err = pgx.BeginFunc(ctx, w.D.Pool, func(tx pgx.Tx) error {
		var state string
		var timeoutS int
		var address *string
		err := tx.QueryRow(ctx, `
			SELECT r.state, j.name, s.name, s.wake_mode, s.address, r.data, r.due_at, r.attempts, r.max_attempts, j.timeout_s
			FROM run r JOIN job j ON j.id = r.job_id JOIN system s ON s.id = j.system_id
			WHERE r.id = $1 FOR UPDATE OF r`, runID).
			Scan(&state, &r.job, &r.system, &r.wakeMode, &address, &r.data, &r.dueAt, &r.attempt, &r.maxAttempts, &timeoutS)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // deleted by retention: nothing to run
		}
		if err != nil {
			return err
		}
		if state != "queued" || r.attempt >= r.maxAttempts {
			return nil
		}
		r.id = runID
		if address != nil {
			r.address = *address
		}
		r.timeout = time.Duration(timeoutS) * time.Second
		r.attempt++
		now := w.D.now()
		if _, err := tx.Exec(ctx, `
			UPDATE run SET state = 'running', attempts = $2, started_at = coalesce(started_at, $3)
			WHERE id = $1`, runID, r.attempt, now); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO attempt (run_id, number, started_at) VALUES ($1, $2, $3) RETURNING id`,
			runID, r.attempt, now).Scan(&r.attemptID); err != nil {
			return err
		}
		ok = true
		return nil
	})
	return r, ok, err
}

// attempt wakes the system if needed and calls it. No database connection
// is held here.
func (w *Worker) attempt(ctx context.Context, r run, log *slog.Logger) outcome {
	if r.wakeMode == schedule.WakeSablier {
		if err := wake.WakeWithin(ctx, w.D.Waker, r.system, w.D.WakeTimeout); err != nil {
			if errors.Is(err, wake.ErrTimeout) {
				return outcome{result: "wake_timeout", err: err.Error()}
			}
			return outcome{result: "error", err: err.Error()}
		}
		awake, stop := context.WithCancel(ctx)
		defer stop()
		go wake.KeepAwakeLoop(awake, w.D.Waker, r.system, w.D.KeepAwakeEvery, log)
	}
	return w.call(ctx, r)
}

// call sends the signed job call and reads the answer (PX-RUN-03).
func (w *Worker) call(ctx context.Context, r run) outcome {
	data := json.RawMessage(r.data)
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	body, err := json.Marshal(jobcall.Call{RunID: r.id, Job: r.job, Attempt: r.attempt, DueAt: r.dueAt.UTC(), Data: data})
	if err != nil {
		return outcome{result: "error", err: "encode call: " + err.Error()}
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	url := w.D.baseURL(r.system, r.address) + "/jobs/" + r.job
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return outcome{result: "error", err: "build call: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if err := w.D.Key.Sign(req, body); err != nil {
		return outcome{result: "error", err: "sign call: " + err.Error()}
	}
	resp, err := w.D.HTTP.Do(req)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return outcome{result: "timeout", err: fmt.Sprintf("no answer within %s", r.timeout)}
		}
		return outcome{result: "error", err: "call failed: " + err.Error()}
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return outcome{result: "success", status: resp.StatusCode}
	}
	return outcome{result: "error", status: resp.StatusCode,
		err: fmt.Sprintf("answered %d: %s", resp.StatusCode, bytes.TrimSpace(answer))}
}

// finish records the attempt and the run's new state in one short
// transaction. final is true when the run has no attempts left.
func (w *Worker) finish(ctx context.Context, r run, out outcome) (final bool, err error) {
	msg := report.TruncateError(out.err)
	var status *int
	if out.status != 0 {
		status = &out.status
	}
	var errText *string
	if msg != "" {
		errText = &msg
	}
	now := w.D.now()
	final = out.result != "success" && r.attempt >= r.maxAttempts
	err = pgx.BeginFunc(ctx, w.D.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE attempt SET ended_at = $2, outcome = $3, status_code = $4, error = $5 WHERE id = $1`,
			r.attemptID, now, out.result, status, errText); err != nil {
			return err
		}
		switch {
		case out.result == "success":
			// The job data is wiped as soon as the run ends (PX-REC-02).
			_, err = tx.Exec(ctx, `
				UPDATE run SET state = 'succeeded', finished_at = $2, data = NULL WHERE id = $1`, r.id, now)
		case final:
			_, err = tx.Exec(ctx, `
				UPDATE run SET state = 'failed', finished_at = $2, data = NULL, last_error = $3 WHERE id = $1`, r.id, now, msg)
		default:
			_, err = tx.Exec(ctx, `
				UPDATE run SET state = 'queued', last_error = $2 WHERE id = $1 AND state = 'running'`, r.id, msg)
		}
		return err
	})
	return final, err
}

// reportFailure tells the monitor once about a run that failed every
// attempt (PX-REP-02), then records when. The report carries names and the
// last error only, never the job data.
func (w *Worker) reportFailure(ctx context.Context, r run, out outcome, log *slog.Logger) {
	if w.D.Reporter == nil {
		return
	}
	var already bool
	if err := w.D.Pool.QueryRow(ctx, `SELECT reported_at IS NOT NULL FROM run WHERE id = $1`, r.id).Scan(&already); err != nil || already {
		return
	}
	f := report.NewFailure(r.system, r.job, r.id, r.attempt, out.err)
	if err := w.D.Reporter.Failure(ctx, f); err != nil {
		log.Error("failure report not sent", "error", err)
		return
	}
	if _, err := w.D.Pool.Exec(ctx, `UPDATE run SET reported_at = $2 WHERE id = $1 AND reported_at IS NULL`, r.id, w.D.now()); err != nil {
		log.Error("failure report sent but not recorded", "error", err)
	}
}
