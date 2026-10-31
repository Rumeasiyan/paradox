package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// runView is a run as the API shows it (contract section 6). Times that
// have not happened yet are null.
type runView struct {
	RunID      string     `json:"run_id"`
	Job        string     `json:"job"`
	State      string     `json:"state"`
	DueAt      time.Time  `json:"due_at"`
	Attempts   int        `json:"attempts"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// querier is a pool or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var errRunNotFound = errors.New("run not found")

// loadRun reads a run and the name of the system that owns it. lock adds
// FOR UPDATE, for use inside a transaction that changes the run.
func loadRun(ctx context.Context, q querier, runID string, lock bool) (runView, string, error) {
	if !uuidRE.MatchString(runID) {
		return runView{}, "", errRunNotFound
	}
	sql := `
		SELECT r.id, j.name, r.state, r.due_at, r.attempts, r.started_at, r.finished_at, s.name
		FROM run r JOIN job j ON j.id = r.job_id JOIN system s ON s.id = j.system_id
		WHERE r.id = $1`
	if lock {
		sql += " FOR UPDATE OF r"
	}
	var v runView
	var system string
	err := q.QueryRow(ctx, sql, runID).Scan(&v.RunID, &v.Job, &v.State, &v.DueAt, &v.Attempts, &v.StartedAt, &v.FinishedAt, &system)
	if errors.Is(err, pgx.ErrNoRows) {
		return runView{}, "", errRunNotFound
	}
	if err != nil {
		return runView{}, "", err
	}
	v.DueAt = v.DueAt.UTC()
	if v.StartedAt != nil {
		t := v.StartedAt.UTC()
		v.StartedAt = &t
	}
	if v.FinishedAt != nil {
		t := v.FinishedAt.UTC()
		v.FinishedAt = &t
	}
	return v, system, nil
}

// answerLoadError writes the answer for a failed loadRun or an ownership
// mismatch and reports whether it wrote one. A run of another system is
// 403 (PX-SIG-05); an unknown or expired run is 404.
func (a *API) answerLoadError(w http.ResponseWriter, c caller, owner string, err error) bool {
	switch {
	case errors.Is(err, errRunNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such run, or it is older than the record keep")
	case err != nil:
		a.Logger.Error("run lookup failed", "system", c.system, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "the run could not be read; try again")
	case owner != c.system:
		a.refuse(w, c, "run belongs to another system")
	default:
		return false
	}
	return true
}
