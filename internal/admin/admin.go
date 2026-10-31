// Package admin answers the operator's commands (PX-REC-03, PX-REC-04).
//
// It listens only on a loopback address inside the container
// (PARADOX_ADMIN_LISTEN), so it needs no signature: whoever can reach it
// is already inside PARADOX's container. The service answers with its own
// database connections, because an operator command opening its own would
// wait forever for PgBouncer's session pool that the service holds.
// Answers never carry job data.
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// MaxLimit caps how many runs one listing returns.
const MaxLimit = 1000

// Admin serves the operator endpoints.
type Admin struct {
	Pool           *pgxpool.Pool
	River          *river.Client[pgx.Tx]
	FailedDataKeep time.Duration // PARADOX_FAILED_DATA_KEEP
	Logger         *slog.Logger
	Now            func() time.Time
}

func (a *Admin) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Run is one run as the operator sees it.
type Run struct {
	RunID      string     `json:"run_id"`
	System     string     `json:"system"`
	Job        string     `json:"job"`
	Source     string     `json:"source"`
	State      string     `json:"state"`
	DueAt      time.Time  `json:"due_at"`
	Attempts   int        `json:"attempts"`
	FinishedAt *time.Time `json:"finished_at"`
	LastError  *string    `json:"last_error"`
}

// Handler returns the admin routes.
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/runs", a.list)
	mux.HandleFunc("POST /admin/runs/{run_id}/retry", a.action("retry", a.retry))
	mux.HandleFunc("POST /admin/runs/{run_id}/cancel", a.action("cancel", a.cancel))
	return mux
}

var states = map[string]bool{"queued": true, "running": true, "succeeded": true, "failed": true, "cancelled": true, "skipped": true, "missed": true}

// list answers GET /admin/runs with optional system, job, state, since
// (a duration back from now, default 24h) and limit (default 50).
func (a *Admin) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since := 24 * time.Hour
	if v := q.Get("since"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "since must be a positive duration such as 2h or 168h")
			return
		}
		since = d
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxLimit {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be 1 to %d", MaxLimit))
			return
		}
		limit = n
	}
	var where []string
	args := []any{a.now().Add(-since)}
	where = append(where, "r.due_at >= $1")
	for _, f := range []struct{ param, column string }{{"system", "s.name"}, {"job", "j.name"}, {"state", "r.state"}} {
		v := q.Get(f.param)
		if v == "" {
			continue
		}
		if f.param == "state" && !states[v] {
			writeError(w, http.StatusBadRequest, "state must be one of queued, running, succeeded, failed, cancelled, skipped, missed")
			return
		}
		args = append(args, v)
		where = append(where, fmt.Sprintf("%s = $%d", f.column, len(args)))
	}
	args = append(args, limit)
	sql := `SELECT r.id, s.name, j.name, r.source, r.state, r.due_at, r.attempts, r.finished_at, r.last_error
		FROM run r JOIN job j ON j.id = r.job_id JOIN system s ON s.id = j.system_id
		WHERE ` + strings.Join(where, " AND ") + fmt.Sprintf(" ORDER BY r.due_at DESC LIMIT $%d", len(args))
	rows, err := a.Pool.Query(r.Context(), sql, args...)
	if err != nil {
		a.fail(w, err)
		return
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Run, error) {
		var x Run
		err := row.Scan(&x.RunID, &x.System, &x.Job, &x.Source, &x.State, &x.DueAt, &x.Attempts, &x.FinishedAt, &x.LastError)
		x.DueAt = x.DueAt.UTC()
		if x.FinishedAt != nil {
			t := x.FinishedAt.UTC()
			x.FinishedAt = &t
		}
		return x, err
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	if runs == nil {
		runs = []Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

func (a *Admin) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, http.ErrHandlerTimeout) {
		writeError(w, http.StatusServiceUnavailable, "timed out")
		return
	}
	a.Logger.Error("admin request failed", "error", err)
	writeError(w, http.StatusInternalServerError, "the request failed; see PARADOX's log")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
