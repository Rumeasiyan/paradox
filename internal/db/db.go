// Package db opens PARADOX's database and sets up River on it.
//
// The reference deployment reaches PostgreSQL through PgBouncer in session
// mode with a pool of 2 server connections (PRD Annex B, links 5 and 6). A
// session-mode client holds a server connection for as long as it is open,
// so PARADOX must never open more client connections than that pool, or
// the extra ones wait forever. River runs with PollOnly: it would otherwise
// hold one of the two connections permanently for LISTEN, leaving one for
// all other work. Polling every second still starts jobs well within the
// 5 seconds of PX-NFR-02. See the decision log (River connection mode).
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// MaxConns is the most connections PARADOX opens; it matches the pool of 2.
const MaxConns = 2

// OpenTimeout is how long Open waits for the database to answer.
const OpenTimeout = 30 * time.Second

// PollInterval is how often River looks for due jobs.
const PollInterval = time.Second

// Open connects to url with at most MaxConns connections and checks the
// database answers.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// The URL usually carries a password, so the error must not repeat it.
		return nil, fmt.Errorf("database URL is not valid")
	}
	cfg.MaxConns = MaxConns
	cfg.MinConns = 0
	// Through PgBouncer, prepared statements cannot be relied on to survive;
	// describe-exec keeps typed parameters without named server statements.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		if errors.Is(pctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			// In session mode a connection waits until a pool slot is free,
			// so a hang here usually means another process holds the pool.
			return nil, fmt.Errorf("database did not answer within %s; is another PARADOX still holding the connection pool? %w", OpenTimeout, err)
		}
		return nil, fmt.Errorf("database not reachable: %w", err)
	}
	return pool, nil
}

// MigrateRiver brings River's own tables up to date. PARADOX's tables are
// migrated separately.
func MigrateRiver(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	_, err = m.Migrate(ctx, rivermigrate.DirectionUp, nil)
	return err
}

// RiverConfig returns the River settings PARADOX always uses, around the
// given workers and queue size.
func RiverConfig(workers *river.Workers, maxWorkers int) *river.Config {
	return &river.Config{
		PollOnly:          true,
		FetchPollInterval: PollInterval,
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: maxWorkers}},
		Workers:           workers,
	}
}
