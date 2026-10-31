package admin

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/Rumeasiyan/paradox/internal/runner"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// actionError is a refusal with its HTTP status.
type actionError struct {
	status int
	msg    string
}

func (e actionError) Error() string { return e.msg }

// action runs fn on the locked run inside one transaction and answers with
// the run afterwards. Every action is recorded as a run action by the
// operator (PX-REC-04).
func (a *Admin) action(name string, fn func(ctx context.Context, tx pgx.Tx, r lockedRun, now time.Time) error) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := req.PathValue("run_id")
		if !uuidRE.MatchString(id) {
			writeError(w, http.StatusNotFound, "no such run")
			return
		}
		ctx := req.Context()
		now := a.now().UTC()
		var out Run
		err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
			var r lockedRun
			err := tx.QueryRow(ctx, `
				SELECT r.id, r.source, r.state, r.attempts, r.finished_at, j.timeout_s, j.retries
				FROM run r JOIN job j ON j.id = r.job_id WHERE r.id = $1 FOR UPDATE OF r`, id).
				Scan(&r.id, &r.source, &r.state, &r.attempts, &r.finishedAt, &r.timeoutS, &r.retries)
			if errors.Is(err, pgx.ErrNoRows) {
				return actionError{http.StatusNotFound, "no such run, or it is older than the record keep"}
			}
			if err != nil {
				return err
			}
			if err := fn(ctx, tx, r, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO run_action (run_id, action, acted_at) VALUES ($1, $2, $3)`, id, name, now); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `
				SELECT r.id, s.name, j.name, r.source, r.state, r.due_at, r.attempts, r.finished_at, r.last_error
				FROM run r JOIN job j ON j.id = r.job_id JOIN system s ON s.id = j.system_id WHERE r.id = $1`, id).
				Scan(&out.RunID, &out.System, &out.Job, &out.Source, &out.State, &out.DueAt, &out.Attempts, &out.FinishedAt, &out.LastError)
		})
		var refused actionError
		switch {
		case errors.As(err, &refused):
			writeError(w, refused.status, refused.msg)
		case err != nil:
			a.fail(w, err)
		default:
			out.DueAt = out.DueAt.UTC()
			a.Logger.Info("operator action", "action", name, "run_id", out.RunID, "system", out.System, "job", out.Job)
			writeJSON(w, http.StatusOK, out)
		}
	}
}

type lockedRun struct {
	id, source, state string
	attempts          int
	finishedAt        *time.Time
	timeoutS, retries int
}

// retry gives a failed run a fresh set of attempts (the job's retries plus
// one), so a passing outage can clear. A failed queued run can be retried
// only while it still holds its data (PARADOX_FAILED_DATA_KEEP); after
// that its data is gone and the system must queue it again.
func (a *Admin) retry(ctx context.Context, tx pgx.Tx, r lockedRun, now time.Time) error {
	if r.state != "failed" {
		return actionError{http.StatusConflict, "only failed runs can be retried; this one is " + r.state}
	}
	if r.source == "queue" && r.finishedAt != nil && r.finishedAt.Before(now.Add(-a.FailedDataKeep)) {
		return actionError{http.StatusConflict, "this queued run's data was deleted after the failed-data keep; ask the system to queue it again"}
	}
	maxAttempts := r.attempts + r.retries + 1
	if _, err := tx.Exec(ctx, `
		UPDATE run SET state = 'queued', max_attempts = $2, finished_at = NULL, reported_at = NULL WHERE id = $1`,
		r.id, maxAttempts); err != nil {
		return err
	}
	_, err := a.River.InsertTx(ctx, tx, runner.RunArgs{RunID: r.id, TimeoutS: r.timeoutS}, &river.InsertOpts{MaxAttempts: r.retries + 1})
	return err
}

// cancel stops a queued run, as the queue API's cancel does, but on the
// operator's behalf.
func (a *Admin) cancel(ctx context.Context, tx pgx.Tx, r lockedRun, now time.Time) error {
	if r.state != "queued" {
		return actionError{http.StatusConflict, "only queued runs can be cancelled; this one is " + r.state}
	}
	if _, err := tx.Exec(ctx, `UPDATE run SET state = 'cancelled', finished_at = $2, data = NULL WHERE id = $1`, r.id, now); err != nil {
		return err
	}
	return runner.CancelRiverJobs(ctx, tx, a.River, r.id)
}
