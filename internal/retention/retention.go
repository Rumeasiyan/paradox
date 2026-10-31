// Package retention deletes what PARADOX must not keep (PX-REC-02,
// PX-NFR-06): run records older than PARADOX_RECORD_KEEP, request IDs older
// than PARADOX_REQUEST_ID_KEEP, and any job data a finished run still holds.
//
// Every statement touches at most BatchSize rows and runs on its own (no
// surrounding transaction), so a large backlog is cleared in many short
// statements. Each one holds a single pool connection briefly, leaving the
// other of the two for River and the API.
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BatchSize is the most rows one statement deletes or updates.
const BatchSize = 1000

// DefaultEvery is the sweep interval used when Loop is given a zero or
// negative one.
const DefaultEvery = time.Hour

// finalStates lists the run states a run never leaves. Runs in any other
// state (queued, running) are never touched here, however old.
const finalStates = `('succeeded', 'failed', 'cancelled', 'skipped', 'missed')`

// Result counts what one Sweep removed.
type Result struct {
	Runs          int64 // runs deleted (their attempts, actions and queue requests go with them)
	QueueRequests int64 // request IDs deleted on their own (their runs stay)
	DataWiped     int64 // finished runs whose leftover data was set to NULL
}

// Removed reports whether the sweep changed anything.
func (r Result) Removed() bool {
	return r.Runs > 0 || r.QueueRequests > 0 || r.DataWiped > 0
}

// Sweep deletes, in batches:
//   - runs in a final state whose finished_at is older than now minus
//     recordKeep; attempts, run actions and queue requests go with them by
//     ON DELETE CASCADE (PX-REC-02);
//   - queue_request rows whose received_at is older than now minus
//     requestIDKeep, leaving their runs (PX-JOB-03);
//   - job data still held by a run in a final state, as a safety net
//     behind the runner and the cancel endpoint, which clear it when a run
//     ends (PX-REC-02, PX-NFR-06).
//
// Queued and running runs are never deleted or changed. A keep of zero or
// less turns that deletion off. On error the counts so far are returned.
func Sweep(ctx context.Context, pool *pgxpool.Pool, now time.Time, recordKeep, requestIDKeep, failedDataKeep time.Duration) (Result, error) {
	var res Result
	var err error
	if recordKeep > 0 {
		res.Runs, err = batched(ctx, pool, `
			DELETE FROM run WHERE id IN (
				SELECT id FROM run
				WHERE finished_at < $2 AND state IN `+finalStates+`
				LIMIT $1)`, now.Add(-recordKeep).UTC())
		if err != nil {
			return res, fmt.Errorf("delete old runs: %w", err)
		}
	}
	if requestIDKeep > 0 {
		res.QueueRequests, err = batched(ctx, pool, `
			DELETE FROM queue_request WHERE (system_id, request_id) IN (
				SELECT system_id, request_id FROM queue_request
				WHERE received_at < $2
				LIMIT $1)`, now.Add(-requestIDKeep).UTC())
		if err != nil {
			return res, fmt.Errorf("delete old request IDs: %w", err)
		}
	}
	// A failed run keeps its data for failedDataKeep so the operator can
	// retry it (PX-REC-02, PX-REC-04); every other finished run loses it.
	res.DataWiped, err = batched(ctx, pool, `
		UPDATE run SET data = NULL WHERE id IN (
			SELECT id FROM run
			WHERE data IS NOT NULL AND (
				state IN ('succeeded', 'cancelled', 'skipped', 'missed')
				OR (state = 'failed' AND finished_at < $2))
			LIMIT $1)`, now.Add(-failedDataKeep).UTC())
	if err != nil {
		return res, fmt.Errorf("wipe finished job data: %w", err)
	}
	return res, nil
}

// batched runs sql with $1 = BatchSize and the further args as $2 onwards,
// until one run touches fewer than BatchSize rows, and returns the total
// touched.
func batched(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) (int64, error) {
	var total int64
	params := append([]any{BatchSize}, args...)
	for {
		tag, err := pool.Exec(ctx, sql, params...)
		if err != nil {
			return total, err
		}
		n := tag.RowsAffected()
		total += n
		if n < BatchSize {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// Loop sweeps at once and then every interval until ctx ends. A failed
// sweep is logged at Error and the loop carries on; a sweep that removed
// anything logs its counts at Info. The logs carry counts only, never job
// data (PX-NFR-06). A nil logger means slog.Default().
func Loop(ctx context.Context, pool *pgxpool.Pool, every, recordKeep, requestIDKeep, failedDataKeep time.Duration, logger *slog.Logger) {
	if every <= 0 {
		every = DefaultEvery
	}
	if logger == nil {
		logger = slog.Default()
	}
	sweep := func() {
		if ctx.Err() != nil {
			return
		}
		res, err := Sweep(ctx, pool, time.Now(), recordKeep, requestIDKeep, failedDataKeep)
		if err != nil {
			if ctx.Err() == nil {
				logger.ErrorContext(ctx, "retention sweep failed", slog.String("error", err.Error()))
			}
			return
		}
		if res.Removed() {
			logger.InfoContext(ctx, "retention sweep removed old records",
				slog.Int64("runs", res.Runs),
				slog.Int64("queue_requests", res.QueueRequests),
				slog.Int64("data_wiped", res.DataWiped))
		}
	}
	sweep()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
