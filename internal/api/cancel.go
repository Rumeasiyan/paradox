package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/Rumeasiyan/paradox/internal/runner"
)

// errCancelNotQueued is a cancel of a run that has already left the queue.
var errCancelNotQueued = errors.New("run is no longer queued")

// errCancelAnswered means the handler has already written the answer.
var errCancelAnswered = errors.New("answer written")

// cancel answers POST /api/jobs/{run_id}/cancel for the owning system
// (PX-JOB-05). In one transaction it marks a queued run cancelled, wipes
// its data (PX-REC-02), records who cancelled it and cancels its River job
// so the system is never woken for it. A worker that already picked the
// run up checks the state under the same row lock and skips it.
func (a *API) cancel(w http.ResponseWriter, r *http.Request, c caller) {
	if len(c.body) != 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "cancel takes an empty body")
		return
	}
	ctx := r.Context()
	var view runView
	err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		v, owner, err := loadRun(ctx, tx, r.PathValue("run_id"), true)
		if a.answerLoadError(w, c, owner, err) {
			return errCancelAnswered
		}
		if v.State != "queued" {
			return errCancelNotQueued
		}
		now := a.now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE run SET state = 'cancelled', finished_at = $2, data = NULL WHERE id = $1`, v.RunID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO run_action (run_id, by_system_id, action, acted_at)
			SELECT $1, id, 'cancel', $3 FROM system WHERE name = $2`, v.RunID, c.system, now); err != nil {
			return err
		}
		if err := runner.CancelRiverJobs(ctx, tx, a.River, v.RunID); err != nil {
			return err
		}
		v.State = "cancelled"
		v.FinishedAt = &now
		view = v
		return nil
	})
	switch {
	case errors.Is(err, errCancelAnswered):
	case errors.Is(err, errCancelNotQueued):
		writeError(w, http.StatusConflict, "conflict", "the run is no longer queued")
	case err != nil:
		a.Logger.Error("cancel failed", "system", c.system, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "the run could not be cancelled; try again")
	default:
		a.Logger.Info("run cancelled", "system", c.system, "job", view.Job, "run_id", view.RunID)
		writeJSON(w, http.StatusOK, view)
	}
}
