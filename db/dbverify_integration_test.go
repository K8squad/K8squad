//go:build db_integration

// Drift regression lane for the standing ownership invariant (ISI-6371), driving the REAL
// dbmigrate.Apply + dbmigrate.Verify + the shipped 0041 reconcile SQL against a real Postgres.
// The CI db lane points DATABASE_URL at a container whose POSTGRES_USER can create roles, so
// the drift injections execute exactly like the documented manual runbook. Skips without
// DATABASE_URL. Pinned behaviors (the ISI-6371 review):
//
//   - Verify detects drift regardless of the ledger (a one-shot 0041 cannot see drift that
//     lands after it applied);
//   - the 0041 repair follows the schema's MAJORITY owner — repairing drifted tables and NEVER
//     moving a healthy one to a drifted peer's owner (the review's inverted-drift failure mode);
//   - the original incident shape (proposal alone drifted) repairs to the majority owner.
package dbmigrate

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

func TestVerifyDetectsAndReconcileRepairsOwnershipDrift(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping ownership-drift integration test (needs a real Postgres)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Bring the schema to HEAD (idempotent: a no-op when an earlier test in this lane already
	// applied it) — 0041 is part of the chain, which also proves it applies clean on fresh DBs.
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	drifts, err := Verify(ctx, db)
	if err != nil {
		t.Fatalf("verify (healthy): %v", err)
	}
	if len(drifts) != 0 {
		t.Fatalf("verify (healthy): want no drift, got %v", drifts)
	}

	// Drift injection needs a second role; without the right to create one (a plain runtime-role
	// DATABASE_URL), the every-boot detection path is still covered by the 0041 companion lane.
	var canCreate bool
	if err := db.QueryRowContext(ctx,
		`SELECT rolsuper OR rolcreaterole FROM pg_roles WHERE rolname = current_user`,
	).Scan(&canCreate); err != nil {
		t.Fatalf("probe role rights: %v", err)
	}
	if !canCreate {
		t.Skip("current role cannot create roles — skipping drift injection (covered by the CI superuser lane)")
	}

	reconcile, err := fs.ReadFile(migrationsFS, "migrations/0041_discussion_proposal_privileges_reconcile.sql")
	if err != nil {
		t.Fatalf("read 0041: %v", err)
	}
	ownerOf := func(table string) string {
		t.Helper()
		var owner string
		if err := db.QueryRowContext(ctx,
			`SELECT tableowner FROM pg_tables WHERE schemaname = 'discussion' AND tablename = $1`, table,
		).Scan(&owner); err != nil {
			t.Fatalf("owner of %s: %v", table, err)
		}
		return owner
	}
	runReconcile := func() {
		t.Helper()
		if _, err := db.ExecContext(ctx, string(reconcile)); err != nil {
			t.Fatalf("run 0041 reconcile: %v", err)
		}
	}
	driftTable := func(table, role string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `ALTER TABLE discussion.`+table+` OWNER TO `+role); err != nil {
			t.Fatalf("drift %s to %s: %v", table, role, err)
		}
	}
	me := ownerOf("proposal")

	cleanup := func() {
		t.Helper()
		_, _ = db.ExecContext(ctx, `ALTER TABLE IF EXISTS discussion.message OWNER TO current_user`)
		_, _ = db.ExecContext(ctx, `ALTER TABLE IF EXISTS discussion.proposal OWNER TO current_user`)
		_, _ = db.ExecContext(ctx, `DROP ROLE IF EXISTS isi6371_probe`)
	}
	defer cleanup()
	if _, err := db.ExecContext(ctx, `DROP ROLE IF EXISTS isi6371_probe`); err != nil {
		t.Fatalf("pre-drop probe role: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE ROLE isi6371_probe NOLOGIN`); err != nil {
		t.Fatalf("create probe role: %v", err)
	}

	// (a) INVERTED drift — the review's failure mode: ONLY discussion.message drifted. The
	// reconcile must repair message to the majority owner and must NOT touch the healthy
	// proposal (the old peer-owner logic moved proposal onto the drifted peer's owner).
	driftTable("message", "isi6371_probe")
	if got := ownerOf("proposal"); got != me {
		t.Fatalf("precondition: proposal owner drifted to %q before the test ran", got)
	}
	drifts, err = Verify(ctx, db)
	if err != nil {
		t.Fatalf("verify (inverted drift): %v", err)
	}
	if !hasDriftTable(drifts, "message") || hasDriftTable(drifts, "proposal") {
		t.Fatalf("verify (inverted drift): want exactly discussion.message flagged, got %v", drifts)
	}
	runReconcile()
	if got := ownerOf("message"); got != me {
		t.Fatalf("inverted drift: message owner = %q, want %q (majority owner)", got, me)
	}
	if got := ownerOf("proposal"); got != me {
		t.Fatalf("inverted drift: proposal owner moved to %q — the reconcile MUST NOT follow a drifted peer (ISI-6371 review issue 1)", got)
	}

	// (b) The INCIDENT shape — proposal alone drifted (the ISI-5051 hand-seeding era): repaired
	// to the majority owner, and Verify flags it before the repair.
	driftTable("proposal", "isi6371_probe")
	drifts, err = Verify(ctx, db)
	if err != nil {
		t.Fatalf("verify (incident drift): %v", err)
	}
	if !hasDriftTable(drifts, "proposal") {
		t.Fatalf("verify (incident drift): want discussion.proposal flagged, got %v", drifts)
	}
	runReconcile()
	if got := ownerOf("proposal"); got != me {
		t.Fatalf("incident drift: proposal owner = %q, want %q", got, me)
	}

	// (c) BOTH sides drifted — the parity-blind spot the review flagged: parity holds between
	// the two drifted tables, so only the majority rule (and the capability anchor) catches it.
	driftTable("message", "isi6371_probe")
	driftTable("proposal", "isi6371_probe")
	drifts, err = Verify(ctx, db)
	if err != nil {
		t.Fatalf("verify (both drifted): %v", err)
	}
	if !hasDriftTable(drifts, "message") || !hasDriftTable(drifts, "proposal") {
		t.Fatalf("verify (both drifted): want message and proposal flagged, got %v", drifts)
	}
	runReconcile()
	if got := ownerOf("message"); got != me {
		t.Fatalf("both drifted: message owner = %q, want %q", got, me)
	}
	if got := ownerOf("proposal"); got != me {
		t.Fatalf("both drifted: proposal owner = %q, want %q", got, me)
	}
	drifts, err = Verify(ctx, db)
	if err != nil {
		t.Fatalf("verify (after repair): %v", err)
	}
	if len(drifts) != 0 {
		t.Fatalf("verify (after repair): want no drift, got %v", drifts)
	}
}

func hasDriftTable(drifts []Drift, table string) bool {
	for _, d := range drifts {
		if d.Schema == "discussion" && d.Table == table {
			return true
		}
	}
	return false
}
