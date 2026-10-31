package api

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// repeatOf finds an earlier request with the same request ID from the same
// system within PARADOX_REQUEST_ID_KEEP and returns its run (PX-JOB-03).
// Placeholder until backlog story 20: a repeated request ID is refused by
// the database instead.
func (a *API) repeatOf(ctx context.Context, tx pgx.Tx, systemID int64, requestID string, now time.Time) (createAnswer, bool, error) {
	return createAnswer{}, false, nil
}
