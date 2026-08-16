//go:build chaos

// Story 12.1 / ISI-2260 — TestSpineProdOutbox: the production §6.2 claim wired
// with WithOutboxCapture() co-commits EXACTLY ONE coord.outbox domain event in
// the SAME transaction as the claim, proved against the SHIPPED schema
// (0001 spine + 0003 outbox) on a real Postgres inside the required chaos gate
// (the workflow's -run 'TestSpine' matches this name too).
//
// This is the same-txn capture teeth (AC-a / C1) on the REAL write path, the
// complement of the pure-Go relay unit tests (pkg/events/relay_test.go) and the
// capture-atomicity integration test (pkg/events/integration_test.go, the
// rolled-back-arm phantom-event teeth):
//
//	O1  N claims with capture ON → exactly N outbox rows, each
//	    entity=work_item / event_type=claimed / project derived from the item /
//	    published_at NULL, and count(outbox) == count(audit claim_acquired):
//	    an event exists IFF its claim committed (no lost, no phantom).
//	O2  capture is OPT-IN: a claimer built WITHOUT the option writes ZERO outbox
//	    rows — the base spine gate (0001-only) is unaffected, no dual-write.
package coord_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

// outboxMigrationSQL locates the shipped 0003 migration, mirroring the candidate
// paths coordMigrationSQL / dispatchMigrationSQL use.
func outboxMigrationSQL(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../db/migrations/0003_coord_outbox.sql",
		"db/migrations/0003_coord_outbox.sql",
	}
	if dir := os.Getenv("COORD_MIGRATIONS_DIR"); dir != "" {
		candidates = append([]string{filepath.Join(dir, "0003_coord_outbox.sql")}, candidates...)
	}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Fatalf("ISI-2663: cannot locate 0003_coord_outbox.sql (looked in %v). "+
		"Refusing to pass without the shipped outbox schema.", candidates)
	return ""
}

func TestSpineProdOutbox(t *testing.T) {
	dsn := dsnOrFatal(t)
	db := openDB(t, dsn)
	ctx := context.Background()

	// Provision 0001 + 0003.
	mustExec(t, db, `DROP SCHEMA IF EXISTS coord CASCADE`)
	mustExec(t, db, coordMigrationSQL(t))
	mustExec(t, db, outboxMigrationSQL(t))

	const n = 8
	seedProdItems(t, db, n)

	// O1 — capture ON: every committed claim co-commits exactly one event.
	claimer, err := coord.NewProdClaimer(db, coord.DefaultProdConfig(), coord.WithOutboxCapture())
	if err != nil {
		t.Fatalf("NewProdClaimer(WithOutboxCapture): %v", err)
	}
	claimed := 0
	for i := 0; i < n; i++ {
		principal := "worker"
		run := prodRunUUID(i + 1)
		_, _, ok, err := claimer.ClaimNext(ctx, principal, run, "")
		if err != nil {
			t.Fatalf("ClaimNext[%d]: %v", i, err)
		}
		if ok {
			claimed++
		}
	}
	if claimed != n {
		t.Fatalf("claimed %d of %d items", claimed, n)
	}

	var outbox, wiClaimed, badTaxonomy, unpublished int
	if err := db.QueryRow(`SELECT count(*) FROM coord.outbox`).Scan(&outbox); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM coord.outbox
		  WHERE entity='work_item' AND event_type='claimed'
		    AND project_id = $1 AND work_item_id IS NOT NULL AND run_id IS NOT NULL`,
		prodTestProject).Scan(&wiClaimed); err != nil {
		t.Fatalf("count well-formed events: %v", err)
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM coord.outbox WHERE published_at IS NOT NULL`).Scan(&unpublished); err != nil {
		t.Fatalf("count published: %v", err)
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM coord.outbox
		  WHERE entity <> 'work_item' OR event_type <> 'claimed'`).Scan(&badTaxonomy); err != nil {
		t.Fatalf("count bad taxonomy: %v", err)
	}
	if outbox != n {
		t.Fatalf("outbox has %d rows, want %d (one per committed claim — same-txn capture)", outbox, n)
	}
	if wiClaimed != n {
		t.Fatalf("well-formed work_item/claimed events = %d, want %d", wiClaimed, n)
	}
	if badTaxonomy != 0 {
		t.Fatalf("%d events with wrong entity/event_type", badTaxonomy)
	}
	if unpublished != 0 {
		t.Fatalf("%d events already stamped published_at — captured rows must start unflushed", unpublished)
	}

	// Cross-check 1:1 with the §6.5 audit row appended in the SAME claim txn: the
	// event lands iff the claim (hence its audit row) committed.
	var auditClaims int
	if err := db.QueryRow(
		`SELECT count(*) FROM coord.audit_log WHERE event_type='claim_acquired'`).Scan(&auditClaims); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditClaims != outbox {
		t.Fatalf("audit claim rows=%d but outbox events=%d — capture is not co-committed 1:1", auditClaims, outbox)
	}

	// O2 — capture OFF (default): a fresh schema + default claimer writes no events.
	mustExec(t, db, `DROP SCHEMA IF EXISTS coord CASCADE`)
	mustExec(t, db, coordMigrationSQL(t))
	mustExec(t, db, outboxMigrationSQL(t))
	seedProdItems(t, db, 1)
	defClaimer, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		t.Fatalf("NewProdClaimer(default): %v", err)
	}
	if _, _, ok, err := defClaimer.ClaimNext(ctx, "worker", prodRunUUID(1), ""); err != nil || !ok {
		t.Fatalf("default ClaimNext: ok=%v err=%v", ok, err)
	}
	var offCount int
	if err := db.QueryRow(`SELECT count(*) FROM coord.outbox`).Scan(&offCount); err != nil {
		t.Fatalf("count outbox (off): %v", err)
	}
	if offCount != 0 {
		t.Fatalf("default claimer emitted %d outbox rows, want 0 (capture is opt-in)", offCount)
	}
}
