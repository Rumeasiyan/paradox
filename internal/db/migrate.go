package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// lockKey is the advisory lock that stops two PARADOX processes (for
// example during a redeploy overlap) migrating at the same time.
const lockKey = 0x50415244 // "PARD"

// Migrate applies PARADOX's own migrations that are not yet recorded, each
// in its own transaction. Files are named NNN_description.sql and applied
// in number order; an applied file must never be edited, only followed by a
// new one.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := migrationList()
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS paradox_migration (
		version    integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("migration table: %w", err)
	}
	for _, f := range files {
		if err := applyOne(ctx, pool, f); err != nil {
			return err
		}
	}
	return nil
}

type migration struct {
	version int
	name    string
}

func migrationList() ([]migration, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := map[int]string{}
	for _, n := range names {
		base := strings.TrimPrefix(n, "migrations/")
		num, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v < 1 {
			return nil, fmt.Errorf("migration %s: name must start with a number and an underscore", base)
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %s and %s share number %d", other, base, v)
		}
		seen[v] = base
		out = append(out, migration{version: v, name: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, m migration) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); err != nil {
			return err
		}
		var done bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM paradox_migration WHERE version = $1)", m.version).Scan(&done); err != nil {
			return err
		}
		if done {
			return nil
		}
		sql, err := migrationFiles.ReadFile(m.name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", strings.TrimPrefix(m.name, "migrations/"), err)
		}
		_, err = tx.Exec(ctx, "INSERT INTO paradox_migration (version) VALUES ($1)", m.version)
		return err
	})
}
