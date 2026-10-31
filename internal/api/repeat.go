package api

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// repeatOf finds an earlier request with the same request ID from the same
// system within PARADOX_REQUEST_ID_KEEP and returns its run (PX-JOB-03).
// The first run is returned even if the repeat names another job or other
// data (contract section 6), with the run's current state.
//
// It first takes a transaction advisory lock on the pair, so two identical
// requests at the same moment are served one after the other: the second
// waits until the first commits and then finds its row, instead of both
// inserting and one failing on the primary key.
//
// A row older than the keep is deleted (its run stays) and ok is false, so
// the caller queues a new run under the same request ID.
func (a *API) repeatOf(ctx context.Context, tx pgx.Tx, systemID int64, requestID string, now time.Time) (createAnswer, bool, error) {
	key := strconv.FormatInt(systemID, 10) + ":" + requestID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return createAnswer{}, false, err
	}
	var runID string
	var received time.Time
	err := tx.QueryRow(ctx, `
		SELECT run_id::text, received_at FROM queue_request
		WHERE system_id = $1 AND request_id = $2`, systemID, requestID).Scan(&runID, &received)
	if errors.Is(err, pgx.ErrNoRows) {
		return createAnswer{}, false, nil
	}
	if err != nil {
		return createAnswer{}, false, err
	}
	if !received.After(now.Add(-a.RequestIDKeep)) {
		_, err := tx.Exec(ctx, `DELETE FROM queue_request WHERE system_id = $1 AND request_id = $2`, systemID, requestID)
		return createAnswer{}, false, err
	}
	v, _, err := loadRun(ctx, tx, runID, false)
	if err != nil {
		// The row cascades away with its run, so a missing run is a fault.
		return createAnswer{}, false, err
	}
	return createAnswer{RunID: v.RunID, Job: v.Job, State: v.State, DueAt: v.DueAt}, true, nil
}
