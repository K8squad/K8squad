package memory

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// applyMigrations applies every embedded forward-only migration exactly once, in lexical order,
// tracking applied versions in memory.schema_migrations. Each migration runs in its own transaction,
// so a crash mid-apply commits or rolls back a whole file, never a partial schema (§7.4). It is
// idempotent — already-applied versions are skipped — and forward-only: it never drops or rewrites a
// previously applied migration.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("enumerate migrations: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no embedded migrations found")
	}
	sort.Strings(entries)

	// The bookkeeping table lives in the memory schema; the memory schema is created by 0001 itself,
	// so create the schema + tracker up front (idempotent, non-destructive).
	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS memory;
		CREATE TABLE IF NOT EXISTS memory.schema_migrations (
			version    text        PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		);`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	for _, name := range entries {
		version := name // full embedded path is a stable, unique key

		var already bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM memory.schema_migrations WHERE version = $1)`,
			version).Scan(&already); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if already {
			continue
		}

		body, err := fs.ReadFile(migrationsFS, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO memory.schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}
	return nil
}
