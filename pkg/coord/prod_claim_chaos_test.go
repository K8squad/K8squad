//go:build chaos

// ISI-2523 — TestSpineProdClaim (P1/P2): the production-bound §6.2 SKIP LOCKED
// claim (prodclaim.go, Story 2.2 / R10 CORE) proved against the SHIPPED coord
// schema (db/migrations/0001_coord_schema.sql) on a real Postgres, inside the
// same required gate as TestSpine (the workflow's -run 'TestSpine' matches this
// name too).
//
// Where C1/C2 prove the guard SHAPE on the int-keyed harness tables, P1/P2
// prove the claim on the schema production actually runs: uuid keys, board
// lanes (§13), PRE-PROVISIONED claim rows (the F3 trigger — no claim seeding),
// and the §6.5 append-only audit log written in the same transaction.
//
//   P1  N open items × M concurrent claimers → no double-claim, no lost work,
//       fence==1 per item, checkout rows name the single winner, audit rows
//       match one-to-one. Differential teeth: the naive check-then-act claim
//       (no SKIP LOCKED, no guard) MUST double-claim first, or the pass means
//       nothing.
//   P2  a claim interrupted at ANY point (ctx deadline sweeps across statement
//       boundaries) never strands a HALF-claimed item — checkout row, board
//       lane and audit either all committed or all absent. Teeth: the same
//       claim split into TWO transactions deterministically strands a
//       half-claimed item, proving the invariant assertion has teeth.
package coord_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

const (
	prodN = 200 // P1: claimable work items
	prodM = 32  // P1: concurrent Runs racing to claim
)

// prodTestProject is the tenancy key all seeded items share (any uuid works;
// the claim path does not filter by project, and one shared project keeps the
// seed a single statement).
const prodTestProject = "11111111-1111-1111-1111-111111111111"

// prodRunUUID builds a deterministic valid uuid for worker/item w, so claim
// rows and audit rows can be matched back to their Run.
func prodRunUUID(w int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", w)
}

// resetProdSchema re-applies the SHIPPED migration into a clean coord schema —
// the teeth bite the real DDL (triggers, PKs, append-only audit) exactly as
// production provisions them.
func resetProdSchema(t testing.TB, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP SCHEMA IF EXISTS coord CASCADE`)
	mustExec(t, db, coordMigrationSQL(t))
}

// seedProdItems inserts n claimable ('todo') items and asserts the F3
// provision trigger gave every one of them exactly one unheld checkout row —
// the structural "exactly one active claim per item" invariant this story
// relies on instead of seeding claim rows by hand.
func seedProdItems(t testing.TB, db *sql.DB, n int) {
	t.Helper()
	mustExec(t, db, `
		INSERT INTO coord.work_item (project_id, title, state, created_by)
		SELECT '`+prodTestProject+`', 'item-' || g, 'todo', 'seed'
		  FROM generate_series(1, $1) g`, n)
	var claims, unheld int
	if err := db.QueryRow(`
		SELECT count(*), count(*) FILTER (WHERE holder_principal IS NULL AND fence_token = 0)
		  FROM coord.claim`).Scan(&claims, &unheld); err != nil {
		t.Fatalf("F3 provision assertion query: %v", err)
	}
	if claims != n || unheld != n {
		t.Fatalf("F3 provision trigger: %d claim rows (%d unheld) for %d items — "+
			"the production claim depends on exactly one pre-provisioned, unheld row per item",
			claims, unheld, n)
	}
}

func TestSpineProdClaim(t *testing.T) {
	dsn := dsnOrFatal(t)
	t.Run("P1_no_double_claim_under_contention", func(t *testing.T) { prodP1NoDoubleClaim(t, dsn) })
	t.Run("P2_interrupted_claim_is_atomic_no_half_state", func(t *testing.T) { prodP2AtomicInterrupt(t, dsn) })
}

// --------------------------- P1 ---------------------------------------------
// The Story 2.2 acceptance, on the shipped schema: N open items, M concurrent
// claimers → every item held by EXACTLY ONE Run. Differential, like C1: the
// naive arm (pick without SKIP LOCKED, acquire without the free-or-expired
// guard — check-then-act) MUST double-claim under the same contention first.
func prodP1NoDoubleClaim(t *testing.T, dsn string) {
	db := openDB(t, dsn)

	// (A) teeth — naive check-then-act MUST double-claim.
	resetProdSchema(t, db)
	seedProdItems(t, db, prodN)
	naive := raceProdClaimers(t, db, prodN, prodM, func(t *testing.T, w int) (string, int64, bool) {
		return naiveProdClaimTxn(t, db, fmt.Sprintf("agent-%d", w), prodRunUUID(w))
	})
	if doubles := prodDoubleClaimed(naive); len(doubles) == 0 {
		t.Fatalf("FALSIFICATION LOST ITS TEETH: naive claim produced no double-claim " +
			"under contention, so a Story 2.2 pass proves nothing (AC discipline, cf. C1). " +
			"Raise contention.")
	}

	// (B) the real §6.2 claim — no double-claim, no lost work, ever.
	resetProdSchema(t, db)
	seedProdItems(t, db, prodN)
	pc, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	good := raceProdClaimers(t, db, prodN, prodM, func(t *testing.T, w int) (string, int64, bool) {
		id, fence, ok, err := pc.ClaimNext(context.Background(), fmt.Sprintf("agent-%d", w), prodRunUUID(w), "")
		if err != nil {
			t.Fatalf("ClaimNext: %v", err)
		}
		return id, fence, ok
	})
	if doubles := prodDoubleClaimed(good); len(doubles) != 0 {
		t.Fatalf("DOUBLE-CLAIM under §6.2 (production schema): %v", doubles)
	}
	assertProdClaimedInvariants(t, db, prodN, good)
}

// assertProdClaimedInvariants closes the loop against the database itself:
// every item claimed exactly once (distinctness + coverage), every checkout row
// naming the recorded winner at fence 1 (monotonic bump from the provisioned 0,
// exactly one claim), every lane advanced to in_progress, and the §6.5 audit
// row present exactly once per claim with the matching fence.
func assertProdClaimedInvariants(t *testing.T, db *sql.DB, n int, res []prodClaimResult) {
	t.Helper()
	winner := map[string]string{} // item id → run uuid
	seen := map[string]bool{}
	for _, r := range res {
		if seen[r.item] {
			t.Fatalf("item %s claimed more than once", r.item)
		}
		seen[r.item] = true
		winner[r.item] = r.run
		if r.fence != 1 {
			t.Fatalf("item %s: fence %d, want 1 (provisioned 0 + exactly one bump)", r.item, r.fence)
		}
	}
	if len(seen) != n {
		t.Fatalf("lost work: %d/%d items claimed (SKIP LOCKED must skip, not lose)", len(seen), n)
	}

	rows, err := db.Query(`
		SELECT w.id::text, w.state, c.holder_principal, c.run_id::text, c.fence_token
		  FROM coord.work_item w
		  JOIN coord.claim c ON c.work_item_id = w.id`)
	if err != nil {
		t.Fatalf("read checkout rows: %v", err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var id, state, holder, run string
		var fence int64
		if err := rows.Scan(&id, &state, &holder, &run, &fence); err != nil {
			t.Fatalf("scan checkout row: %v", err)
		}
		if state != "in_progress" {
			t.Fatalf("item %s: lane %q after the race, want in_progress", id, state)
		}
		if holder == "" || run != winner[id] {
			t.Fatalf("item %s: checkout row names (%q,%q), want the single recorded winner run %q",
				id, holder, run, winner[id])
		}
		if fence != 1 {
			t.Fatalf("item %s: fence_token %d in db, want 1", id, fence)
		}
		checked++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("checkout rows iteration: %v", err)
	}
	if checked != n {
		t.Fatalf("checkout row join covered %d items, want %d", checked, n)
	}

	var audits, distinctAudits, fenceMismatches int
	// Explicit error check (unlike the fire-and-forget mustQueryRow): a broken
	// assertion query must fail loud, never read as "zero rows".
	if err := db.QueryRow(`
		SELECT count(*), count(DISTINCT a.work_item_id),
		       count(*) FILTER (WHERE a.fence_token <> c.fence_token)
		  FROM coord.audit_log a
		  JOIN coord.claim c ON c.work_item_id = a.work_item_id
		 WHERE a.event_type = 'claim_acquired'`).Scan(&audits, &distinctAudits, &fenceMismatches); err != nil {
		t.Fatalf("audit assertion query: %v", err)
	}
	if audits != n || distinctAudits != n || fenceMismatches != 0 {
		t.Fatalf("audit: %d claim_acquired rows (%d distinct, %d fence mismatches), "+
			"want exactly one per item (%d) matching each checkout fence", audits, distinctAudits, fenceMismatches, n)
	}
}

// --------------------------- P2 ---------------------------------------------
// "…in ONE transaction": an interrupted claim must never strand a HALF-claimed
// item (checkout row rewritten but lane unchanged, or lane moved without a
// checkout, or either without its audit row). A deadline sweep across the
// single-tx claim interrupts at every stage; the invariant must hold for every
// outcome. Teeth: the same claim split into TWO transactions, with the second
// cancelled, deterministically strands the half-claimed item the invariant
// exists to catch.
func prodP2AtomicInterrupt(t *testing.T, dsn string) {
	db := openDB(t, dsn)

	// (A) teeth — a two-transaction claim MUST be catchable as half-claimed.
	resetProdSchema(t, db)
	seedProdItems(t, db, 1)
	if !naiveProdHalfClaim(t, db) {
		t.Fatal("FALSIFICATION LOST ITS TEETH: the split claim did not strand a " +
			"half-claimed item, so the atomicity assertion proves nothing")
	}

	// (B) the real single-transaction claim under a deadline sweep. Every
	// attempt either completes before its deadline or is cancelled mid-flight
	// at some statement boundary; whichever happens, no item may end up
	// half-claimed. The sweep repeats each delay so the interruption points
	// cover the whole statement sequence.
	resetProdSchema(t, db)
	const items = 40
	seedProdItems(t, db, items)
	pc, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	// The sweep must span from "interrupted before the first statement" all the
	// way to "comfortably longer than a full claim commit" so BOTH sides of the
	// invariant are exercised. The short end (µs..low-ms) probes every mid-flight
	// interruption point. The long end must exceed the claim's commit latency on
	// the ACTUAL engine — and a single ClaimNext against real CNPG reached over a
	// kubectl port-forward is an order of magnitude slower than the bare
	// postgres:16 container this suite used to run on, so an 8ms ceiling now fires
	// before every commit and the "claimed side" is never reached (ISI-2918). The
	// graded tail up to seconds guarantees several attempts always commit
	// regardless of engine latency, without weakening the short-deadline probes.
	delays := []time.Duration{
		0, 50 * time.Microsecond, 100 * time.Microsecond, 250 * time.Microsecond,
		500 * time.Microsecond, time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond,
		8 * time.Millisecond, 16 * time.Millisecond, 32 * time.Millisecond, 64 * time.Millisecond,
		125 * time.Millisecond, 250 * time.Millisecond, time.Second, 2 * time.Second,
	}
	for i := 0; i < items; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), delays[i%len(delays)])
		// The outcome (claimed / nothing / ctx error) is irrelevant — the
		// invariant below must hold for every possible one.
		_, _, _, _ = pc.ClaimNext(ctx, "interrupted-agent", prodRunUUID(i), "")
		cancel()
	}
	assertNoHalfClaimed(t, db)
	// The sweep must actually have exercised BOTH sides of the invariant: if
	// every attempt was cancelled before commit, "no half-claim" passed
	// trivially and proved nothing. At least one item must be fully claimed.
	var claimed int
	if err := db.QueryRow(`SELECT count(*) FROM coord.work_item WHERE state = 'in_progress'`).Scan(&claimed); err != nil {
		t.Fatalf("claimed-count query: %v", err)
	}
	if claimed == 0 {
		t.Fatal("P2 sweep never completed a single claim — every deadline fired before commit; " +
			"widen the delay sweep so the claimed side of the invariant is actually exercised")
	}
}

// assertNoHalfClaimed is the §6.2 one-transaction invariant over the whole
// coord schema: every item is EITHER fully unclaimed (todo lane, unheld
// checkout row, fence 0, no audit row) OR fully claimed (in_progress lane,
// held checkout row, fence ≥ 1, and exactly one claim_acquired audit row per
// fence bump). Anything in between is a stranded half-claim.
func assertNoHalfClaimed(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`
		SELECT w.id::text, w.state, c.holder_principal IS NULL, c.fence_token,
		       (SELECT count(*) FROM coord.audit_log a
		         WHERE a.work_item_id = w.id AND a.event_type = 'claim_acquired')
		  FROM coord.work_item w
		  JOIN coord.claim c ON c.work_item_id = w.id`)
	if err != nil {
		t.Fatalf("read items for half-claim scan: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, state string
		var unheld bool
		var fence int64
		var audits int
		if err := rows.Scan(&id, &state, &unheld, &fence, &audits); err != nil {
			t.Fatalf("scan half-claim row: %v", err)
		}
		fullyUnclaimed := state == "todo" && unheld && fence == 0 && audits == 0
		fullyClaimed := state == "in_progress" && !unheld && fence >= 1 && int64(audits) == fence
		if !fullyUnclaimed && !fullyClaimed {
			t.Fatalf("HALF-CLAIMED item stranded (claim not one transaction): "+
				"item %s state=%s unheld=%v fence=%d claim_acquired_audits=%d "+
				"— want (todo, unheld, 0, 0) or (in_progress, held, ≥1, ==fence)", id, state, unheld, fence, audits)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("half-claim scan iteration: %v", err)
	}
}

// naiveProdHalfClaim executes the checkout rewrite and the lane advance as TWO
// transactions, cancelling the second: the item ends up held at fence 1 but
// still in the todo lane. Returns true iff that half-claimed state is visible.
func naiveProdHalfClaim(t *testing.T, db *sql.DB) bool {
	t.Helper()
	mustExec(t, db, `
		UPDATE coord.claim
		   SET holder_principal = 'zombie', run_id = '00000000-0000-0000-0000-00000000dead',
		       fence_token = fence_token + 1,
		       lease_expires_at = clock_timestamp() + interval '30 seconds',
		       acquired_at = clock_timestamp()
		 WHERE work_item_id = (SELECT id FROM coord.work_item WHERE state = 'todo' LIMIT 1)`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the lane advance dies before it starts — the crash point
	if _, err := db.ExecContext(ctx,
		`UPDATE coord.work_item SET state = 'in_progress'
		  WHERE id = (SELECT id FROM coord.work_item WHERE state = 'todo' LIMIT 1) AND state = 'todo'`); err == nil {
		t.Fatal("teeth: the pre-cancelled lane advance unexpectedly succeeded — the split-claim " +
			"probe cannot demonstrate the half-claimed window")
	}
	var half int
	mustQueryRow(t, db, `
		SELECT count(*) FROM coord.work_item w JOIN coord.claim c ON c.work_item_id = w.id
		 WHERE w.state = 'todo' AND c.holder_principal IS NOT NULL`).Scan(&half)
	return half == 1
}

// ===========================================================================
// Contention driver + naive teeth arm for the production schema (P1).
// ===========================================================================

type prodClaimResult struct {
	item  string
	run   string
	fence int64
}

// raceProdClaimers drives m goroutines racing a claim function until the todo
// lane is exhausted — maximising contention by starting everyone together.
func raceProdClaimers(t *testing.T, db *sql.DB, n, m int,
	claim func(t *testing.T, w int) (item string, fence int64, ok bool)) []prodClaimResult {
	t.Helper()
	var mu sync.Mutex
	var out []prodClaimResult
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < m; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for {
				item, fence, ok := claim(t, w)
				if !ok {
					if prodClaimableCount(t, db) == 0 {
						return
					}
					continue // contended this pass, not exhausted — retry
				}
				mu.Lock()
				out = append(out, prodClaimResult{item, prodRunUUID(w), fence})
				mu.Unlock()
			}
		}(w)
	}
	close(start)
	wg.Wait()
	// The count check is deliberately NOT here: the naive teeth arm
	// double-claims by design (more results than items — that is the bug it
	// exists to show), while the real arm's exact coverage and distinctness
	// are asserted by the caller (assertProdClaimedInvariants).
	return out
}

func prodClaimableCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var c int
	mustQueryRow(t, db, `SELECT count(*) FROM coord.work_item WHERE state = 'todo'`).Scan(&c)
	return c
}

func prodDoubleClaimed(res []prodClaimResult) map[string][]string {
	holders := map[string][]string{}
	for _, r := range res {
		holders[r.item] = append(holders[r.item], r.run)
	}
	doubles := map[string][]string{}
	for item, hs := range holders {
		if len(hs) > 1 {
			doubles[item] = hs
		}
	}
	return doubles
}

// naiveProdClaimTxn is the anti-pattern Story 2.2 forbids, on the production
// tables: pick the first todo item with NO row lock, then rewrite its checkout
// row with NO free-or-expired guard (check-then-act). Under contention two
// claimers read the same open row and both "win" → double-claim.
func naiveProdClaimTxn(t *testing.T, db *sql.DB, principal, run string) (string, int64, bool) {
	t.Helper()
	ctx := context.Background()
	var id string
	err := db.QueryRowContext(ctx, `
		SELECT w.id::text FROM coord.work_item w JOIN coord.claim c ON c.work_item_id = w.id
		 WHERE w.state = 'todo' ORDER BY w.created_at, w.id LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", 0, false
	}
	if err != nil {
		t.Fatalf("naive pick: %v", err)
	}
	time.Sleep(0) // widen the read→write window so the race bites
	var fence int64
	// NO SKIP LOCKED on the pick and NO holder/lease guard here — the bug.
	if err := db.QueryRowContext(ctx, `
		UPDATE coord.claim
		   SET holder_principal = $1, run_id = $2::uuid,
		       fence_token = fence_token + 1,
		       lease_expires_at = clock_timestamp() + interval '30 seconds',
		       acquired_at = clock_timestamp()
		 WHERE work_item_id = $3::uuid
		 RETURNING fence_token`, principal, run, id).Scan(&fence); err != nil {
		t.Fatalf("naive acquire: %v", err)
	}
	mustExec(t, db, `UPDATE coord.work_item SET state = 'in_progress' WHERE id = $1::uuid AND state = 'todo'`, id)
	return id, fence, true
}
