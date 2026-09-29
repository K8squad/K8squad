//go:build db_integration

// Integration test for the shared-schema migration runner against a REAL, FRESH Postgres (ISI-5051).
// Build-tag gated so it never runs in the default unit lane; CI provisions Postgres and runs
//
//	go test -tags=db_integration ./db/...
//
// It requires a database with NONE of the auth/coord/scm/discussion schemas yet (a freshly-provisioned
// store) — Apply is forward-only and deliberately does not auto-baseline a pre-existing schema. When
// DATABASE_URL is unset the test SKIPS, so a developer without Postgres is not blocked.
//
// It proves the whole ISI-5051 contract end to end: a fresh DB reaches HEAD in one Apply; the ledger
// records every applied version; a second Apply is a clean no-op (idempotent, every-roll safe); and a
// DB left one migration behind HEAD (the ISI-4919 drift shape) is caught up by the next Apply.
package dbmigrate

import (
	"context"
	"database/sql"
	"os"
	"path"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

func openFreshTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping the dbmigrate integration test (needs a fresh real Postgres)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func ledgerCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM public.schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	return n
}

func TestApplyFreshThenIdempotent(t *testing.T) {
	db := openFreshTestDB(t)
	ctx := context.Background()

	names, err := selectMigrations()
	if err != nil {
		t.Fatalf("selectMigrations: %v", err)
	}

	// 1. Fresh DB → one Apply reaches HEAD and records every version.
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if got := ledgerCount(t, db); got != len(names) {
		t.Fatalf("ledger has %d versions after first Apply; want %d", got, len(names))
	}
	// The named-outage sentinels must exist (ISI-4919): discussion.message.audience and proposal table.
	var audience, proposal bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM information_schema.columns
		WHERE table_schema='discussion' AND table_name='message' AND column_name='audience')`).Scan(&audience); err != nil {
		t.Fatalf("probe audience: %v", err)
	}
	if err := db.QueryRow(`SELECT to_regclass('discussion.proposal') IS NOT NULL`).Scan(&proposal); err != nil {
		t.Fatalf("probe proposal: %v", err)
	}
	if !audience || !proposal {
		t.Fatalf("HEAD schema missing ISI-4919 objects: audience=%v proposal=%v", audience, proposal)
	}

	// 2. Second Apply is a clean no-op (every-roll idempotency).
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got := ledgerCount(t, db); got != len(names) {
		t.Fatalf("ledger changed on idempotent re-run: %d != %d", got, len(names))
	}

	// 3. Drift shape: delete HEAD from the ledger AND drop its object, then Apply catches it back up.
	// HEAD is 0028_comment_payload.sql — its object is coord.comment.payload (ISI-5214). The dropped
	// object MUST be the one the HEAD migration creates, so re-running HEAD alone heals the drift
	// (this section was stale at 0025's discussion.proposal and broke every time HEAD advanced).
	head := path.Base(names[len(names)-1]) // 0028_comment_payload.sql
	if _, err := db.Exec(`DELETE FROM public.schema_migrations WHERE version=$1`, head); err != nil {
		t.Fatalf("simulate drift (delete ledger row): %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE coord.comment DROP COLUMN IF EXISTS payload`); err != nil {
		t.Fatalf("simulate drift (drop object): %v", err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("Apply after drift: %v", err)
	}
	var payloadCol bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM information_schema.columns
		WHERE table_schema='coord' AND table_name='comment' AND column_name='payload')`).Scan(&payloadCol); err != nil {
		t.Fatalf("re-probe payload column: %v", err)
	}
	if !payloadCol {
		t.Fatal("Apply did not re-create the dropped HEAD object — drift not healed")
	}
	if got := ledgerCount(t, db); got != len(names) {
		t.Fatalf("ledger not back at HEAD after drift heal: %d != %d", got, len(names))
	}
}
