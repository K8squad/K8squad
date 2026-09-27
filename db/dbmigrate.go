// Package dbmigrate is the apiserver's forward-only migration runner for the shared top-level
// schemas (auth, coord, scm, discussion). It embeds db/migrations/*.sql and applies every
// migration exactly once, in filename order, tracking applied versions in a public.schema_migrations
// ledger — the same self-healing discipline internal/memory already uses for the memory schema
// (see internal/memory/migrate.go).
//
// WHY (ISI-5051): before this runner, db/migrations/*.sql had NO automated runner. Each file was
// applied by hand on every cluster, so a newly-landed migration silently drifted until a feature
// broke in production. The concrete outage (ISI-4919): 0024/0025 were never applied to
// k8squad-system / k8squad-preview and the discussion room 500'd on `openThread`
// (`column "audience" does not exist`). This closes that drift class permanently: on every apiserver
// start, HEAD is applied automatically and recorded, so a missing migration can never survive a roll.
//
// DISCIPLINE (db/migrations/README.md):
//   - Forward-only: no down migrations; a mistake is corrected by a new forward file.
//   - Applied once, in lexical filename order, each inside its own transaction — a crash mid-apply
//     commits or rolls back a whole file, never a partial schema.
//   - Idempotent: an already-recorded version is skipped, so re-runs (every roll) are no-ops.
//   - `*_test.sql` companions are runnable self-checks, NOT migrations — the runner ignores them.
package dbmigrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"path"
	"sort"
	"strings"
)

// migrationsFS embeds the shared forward-only SQL migrations so the apiserver binary applies HEAD on
// start. It embeds db/migrations/*.sql relative to this file. The `*_test.sql` self-check companions
// are embedded too but filtered out by Apply — see selectMigrations.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Apply runs every embedded forward-only migration exactly once, in lexical filename order, tracking
// applied versions in public.schema_migrations. It is idempotent (already-applied versions are
// skipped) and forward-only (it never drops or rewrites a previously applied migration). Each
// migration runs in its own transaction, so a crash mid-apply never leaves a partial schema.
//
// It logs loudly: the ledger HEAD at start, every migration it applies, and the final HEAD — so a
// drifted DB (one behind HEAD) is visible in the apiserver's startup logs, and a clean roll prints a
// one-line "already at HEAD" confirmation.
//
// NOTE ON ADOPTION (ISI-5051): the two long-lived DBs (k8squad-system, k8squad-preview) were
// reconciled to HEAD and their ledgers seeded by hand when this runner shipped, so Apply is a no-op
// there on the first roll. A freshly-provisioned DB starts with an empty ledger and applies the full
// history in order. This runner deliberately does NOT auto-baseline (mark unrun migrations as
// applied) on a pre-existing schema with an empty ledger: that would freeze exactly the kind of
// non-contiguous drift ISI-5051 exists to eliminate. Introducing the runner to any other pre-existing
// DB therefore requires seeding public.schema_migrations to its true applied state first.
func Apply(ctx context.Context, db *sql.DB) error {
	names, err := selectMigrations()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("dbmigrate: no embedded migrations found")
	}

	// The ledger lives in the always-present public schema; create it up front (idempotent,
	// non-destructive) so applied state is queryable even before the first migration.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS public.schema_migrations (
			version    text        PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		);`); err != nil {
		return fmt.Errorf("dbmigrate: ensure schema_migrations: %w", err)
	}

	applied := 0
	for _, name := range names {
		version := path.Base(name) // stable, human-readable ledger key, e.g. 0025_discussion_proposal.sql

		var already bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version = $1)`,
			version).Scan(&already); err != nil {
			return fmt.Errorf("dbmigrate: check %s: %w", version, err)
		}
		if already {
			continue
		}

		body, err := fs.ReadFile(migrationsFS, name)
		if err != nil {
			return fmt.Errorf("dbmigrate: read %s: %w", name, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("dbmigrate: begin %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("dbmigrate: apply %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO public.schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("dbmigrate: record %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("dbmigrate: commit %s: %w", version, err)
		}
		applied++
		log.Printf("dbmigrate: applied %s", version)
	}

	head := path.Base(names[len(names)-1])
	if applied == 0 {
		log.Printf("dbmigrate: schema already at HEAD (%s); %d migrations tracked", head, len(names))
	} else {
		log.Printf("dbmigrate: applied %d migration(s); schema now at HEAD (%s)", applied, head)
	}
	return nil
}

// selectMigrations returns the embedded migration paths (migrations/NNNN_*.sql) in lexical order,
// excluding the *_test.sql self-check companions, which are plain-SQL assertions, not migrations.
func selectMigrations() ([]string, error) {
	entries, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: enumerate migrations: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e, "_test.sql") {
			continue
		}
		out = append(out, e)
	}
	sort.Strings(out)
	return out, nil
}
