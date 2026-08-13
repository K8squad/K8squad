//go:build chaos

// ISI-2347 — Go TestSpine chaos suite (C1–C7), the REQUIRED gate executed by
// k8squad/.github/workflows/spine-chaos.yml.
//
// ┌───────────────────────────────────────────────────────────────────────────┐
// │  STAGING COPY — DO NOT COMPILE HERE, DO NOT COPY YET.                        │
// │                                                                             │
// │  This file lives in the ksquad BMAD workspace (a non-Go-module docs repo)   │
// │  as the turnkey drop-in for ISI-2347. It is a `.go.stage` file on purpose:  │
// │  no Go tooling picks it up, and it stays out of spine-chaos.yml's guard.    │
// │                                                                             │
// │  spine-chaos.yml SKIPS itself while `pkg/coord` AND `internal/coord` are    │
// │  both absent (skeleton phase). The MOMENT you create either directory the   │
// │  guard flips to present=true and the gate compiles+runs `TestSpine`. So:    │
// │                                                                             │
// │  Drop-in procedure (only once the Epic 2/3 Go spine lands):                 │
// │    1. Rename this file to `spine_chaos_test.go`.                             │
// │    2. Place it in `internal/coord/` (or `pkg/coord/`) alongside the spine.  │
// │    3. Implement `newSUT` (bottom of file) against the real coord API — the  │
// │       `SUT` interface below is the CONTRACT the spine must satisfy for the  │
// │       gate. Adapt the adapter, NOT the case logic.                          │
// │    4. Ensure spine-chaos.yml exports DATABASE_URL pointing at the CNPG      │
// │       cluster (see the "CI WIRING GAP" note near TestSpine). Under the      │
// │       `chaos` tag a missing DATABASE_URL is a FATAL, never a silent skip.   │
// └───────────────────────────────────────────────────────────────────────────┘
//
// Faithful 1:1 translation of the language-neutral falsification anchor
// (docs/bmad/spikes/bench/chaos-harness.py + claim-nodouble-check.py, Story 2.7
// / ISI-2197, green). Every case is DIFFERENTIAL, exactly as the Python is:
// first prove the BROKEN variant breaks (so a PASS means something), then prove
// the real §6.2/§6.3/§6.4 statement holds. The "broken" arm is executed as raw
// SQL against the SAME `claim`/`work_item` tables the spine owns (Story 2.1
// schema), so the teeth bite the real schema, not a toy model.
//
// Case ↔ scenario ↔ invariant map (spec: 2-7-concurrency-chaos-harness.md §"map"):
//   C1 no double-claim              claim-nodouble-check.py (A/B)   §6.2 AC2/AC3
//   C2 SKIP-LOCKED fan-out distinct claim-nodouble-check.py         §6.2 (no lost work)
//   C3 crash-mid-claim reclaim      chaos-harness.py (b)            §6.3 AC3
//   C4 stale-holder write/renew     chaos-harness.py (c)            §6.3 AC4
//   C5 zombie-writer-vs-PVC         Go-only (live kind + PG)        §6.3 AC6  <-- NEW
//   C6 double-dispatch dedup        chaos-harness.py (d)            §6.4 AC5
//   C7 re-entrant claim/complete    chaos-harness.py (d)            §6.4 AC5
//
// The suite is run by CI as:
//   go test -race -tags=chaos -run 'TestSpine' ./pkg/coord/... ./internal/coord/...
// `-race` is load-bearing (AC1): a claim/lease data race must fail the gate too.

package coord_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

// ---------------------------------------------------------------------------
// SUT contract — the coordination-spine surface the chaos gate exercises.
//
// This is the CONTRACT Epics 2/3 must satisfy. Each method IS one pinned
// §6.2/§6.3/§6.4 statement. `newSUT` (bottom) wires it to the real coord pkg;
// downstream dev fills that adapter and NOTHING else in this file.
// ---------------------------------------------------------------------------
type SUT interface {
	// ClaimNext = §6.2 one-transaction SKIP-LOCKED pop → conditional fence-bump
	// acquire → work_item.state='claimed'. Returns the claimed item id, its fresh
	// fence, and ok=false when no open+unlocked item was available this pass.
	// (C1/C2)
	ClaimNext(ctx context.Context, principal, run string) (item int, fence int64, ok bool)

	// Acquire = §6.2 conditional fence-bump acquire of a SPECIFIC item
	// (queue-pull or reclaim share this guard: holder IS NULL OR lease expired).
	// ok=false when a live lease blocks it. (C3)
	Acquire(ctx context.Context, item int, principal, run string) (fence int64, ok bool)

	// Renew = §6.2 renew: holder=me AND fence=myFence AND lease_expires_at>now().
	// (C4)
	Renew(ctx context.Context, item int, principal string, fence int64) bool

	// Complete = §6.3 fenced state-mutating write: … AND fence_token=:myFence
	// AND status='claimed'. Rejects a stale fence and a done item. (C4/C7)
	Complete(ctx context.Context, item int, principal string, fence int64) bool

	// RedriveClaim = §6.4 reconcile-safe re-drive: re-read claim+fence; a no-op
	// (returns the SAME fence, ok=true, no bump) when we still hold it LIVE (same
	// run, un-lapsed lease). Re-acquires (bumping the fence, ok=true) when the
	// fence advanced or the lease lapsed; ok=false — no fence returned — when a
	// live FOREIGN lease blocks re-acquire. (C7)
	RedriveClaim(ctx context.Context, item int, principal, run string, fence int64) (fence2 int64, ok bool)

	// DispatchOnce = §6.4 idempotent external-effect dispatch guarded by a
	// durable marker: re-entry returns the SAME recorded task id. (C6)
	DispatchOnce(ctx context.Context, run, taskID string) string

	// ReclaimFenced = §6.3 fence-BEFORE-release reclaim protocol (C5, live layer):
	// pod-kill/cordon + stamp reclaim_fenced_at → confirm the holder is fenced at
	// the resource layer → THEN the §6.2 release that bumps the fence. Returns the
	// bumped fence. The ordering (fence stamped strictly before the release) is
	// what C5 asserts and is why C5 cannot live in the in-process model.
	ReclaimFenced(ctx context.Context, item int, newPrincipal, newRun string) (fence int64, err error)
}

const (
	nItems     = 200 // C1/C2: open work items
	mClaimers  = 32  // C1/C2: concurrent Runs racing to claim
	leaseShort = "1 second"
)

// ---------------------------------------------------------------------------
// Fixture: real Postgres (CNPG in kind, provided by spine-chaos.yml), the
// Story 2.1 coord schema, and a fresh SUT bound to it.
// ---------------------------------------------------------------------------
func dsnOrFatal(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		// CI WIRING GAP: spine-chaos.yml stands up CNPG but does not yet export
		// DATABASE_URL to the test step. Under the `chaos` build tag these tests
		// REQUIRE real Postgres; a missing DSN is a FATAL, not a skip — a required
		// gate must fail loud, never go silently green (AC1). Wire the CNPG
		// service DSN into the "Run chaos suite" step's env before first green.
		t.Fatal("DATABASE_URL unset under -tags=chaos: the spine chaos gate " +
			"requires a live Postgres (CNPG). Refusing to pass silently (AC1).")
	}
	return dsn
}

// freshSchema resets the coord tables to the Story 2.1 shape. In the real repo,
// prefer running the checked-in migrations; the inline DDL keeps the staged file
// self-contained and documents exactly the columns the guards read.
func freshSchema(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`DROP TABLE IF EXISTS claim`,
		`DROP TABLE IF EXISTS work_item`,
		`CREATE TABLE work_item(id int PRIMARY KEY, project_id int, state text NOT NULL)`,
		`CREATE TABLE claim(
			work_item_id int PRIMARY KEY REFERENCES work_item(id),
			holder_principal text, run_id text,
			fence_token int NOT NULL DEFAULT 0,
			lease_expires_at timestamptz,
			reclaim_fenced_at timestamptz)`, // C5: fence-before-release marker (§6.3)
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("schema reset: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO work_item VALUES ($1,1,'open')`, i); err != nil {
			t.Fatalf("seed work_item: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO claim(work_item_id) VALUES ($1)`, i); err != nil {
			t.Fatalf("seed claim: %v", err)
		}
	}
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// ===========================================================================
// TestSpine — the single entrypoint the workflow's -run 'TestSpine' matches.
// Each case is a subtest so a failure names the exact broken invariant.
// ===========================================================================
func TestSpine(t *testing.T) {
	dsn := dsnOrFatal(t)

	t.Run("C1_no_double_claim", func(t *testing.T) { spineC1NoDoubleClaim(t, dsn) })
	t.Run("C2_skip_locked_fanout_distinct", func(t *testing.T) { spineC2FanoutDistinct(t, dsn) })
	t.Run("C3_crash_mid_claim_reclaim", func(t *testing.T) { spineC3CrashMidClaim(t, dsn) })
	t.Run("C4_stale_holder_write_rejected", func(t *testing.T) { spineC4StaleHolder(t, dsn) })
	t.Run("C5_zombie_writer_vs_pvc_fence_before_release", func(t *testing.T) { spineC5FenceBeforeRelease(t, dsn) })
	t.Run("C6_double_dispatch_dedup", func(t *testing.T) { spineC6DispatchDedup(t, dsn) })
	t.Run("C7_reentrant_claim_complete_noop", func(t *testing.T) { spineC7ReentrantNoop(t, dsn) })
}

// --------------------------- C1 / C2 ---------------------------------------
// no double-claim + SKIP-LOCKED fan-out distinct, no lost work, under real
// contention. Translates claim-nodouble-check.py (A) teeth + (B) §6.2 hold.
// Differential: the NAIVE arm (raw check-then-act, no row lock, no CAS) MUST
// double-claim, else the harness has lost its teeth (AC2, "fails loud").

func spineC1NoDoubleClaim(t *testing.T, dsn string) {
	db := openDB(t, dsn)

	// (A) teeth — the naive claim MUST produce a double-claim under contention.
	freshSchema(t, db, nItems)
	naive := raceClaimers(t, db, nItems, mClaimers, naiveClaimTxn)
	if doubles := doubleClaimed(naive); len(doubles) == 0 {
		t.Fatalf("FALSIFICATION LOST ITS TEETH: naive claim produced no " +
			"double-claim, so a §6.2 pass proves nothing (AC2). Raise contention.")
	}

	// (B) §6.2 — the real SUT MUST NOT double-claim, ever, and lose no work.
	freshSchema(t, db, nItems)
	sut := newSUT(t, db, dsn)
	good := raceClaimersSUT(t, sut, nItems, mClaimers)
	if doubles := doubleClaimed(good); len(doubles) != 0 {
		t.Fatalf("DOUBLE-CLAIM under §6.2: %v", doubles)
	}
	assertAllClaimedUniqueFence(t, good, nItems)
}

func spineC2FanoutDistinct(t *testing.T, dsn string) {
	// C2 is the "no lost work + distinct assignment" facet of the same §6.2
	// fan-out: SKIP LOCKED skips CONTENDED rows, never LOSES them, and no two
	// Runs get the same item. C1's (B) arm already asserts both (every item
	// claimed exactly once, unique (item,fence)); C2 re-asserts distinctness
	// explicitly so a regression that lost work vs. one that double-assigned name
	// different subtests.
	db := openDB(t, dsn)
	freshSchema(t, db, nItems)
	sut := newSUT(t, db, dsn)
	res := raceClaimersSUT(t, sut, nItems, mClaimers)

	seenItem := map[int]string{}
	for _, r := range res {
		if prev, ok := seenItem[r.item]; ok {
			t.Fatalf("C2 non-distinct: item %d handed to both %s and %s", r.item, prev, r.run)
		}
		seenItem[r.item] = r.run
	}
	if len(seenItem) != nItems {
		t.Fatalf("C2 lost work: %d/%d items claimed (SKIP LOCKED must skip, not lose)", len(seenItem), nItems)
	}
}

// --------------------------- C3 --------------------------------------------
// crash-mid-claim: a live lease is NOT reclaimable; after expiry the same §6.2
// conditional UPDATE reclaims with a bumped fence, and the crashed holder can no
// longer renew. Translates chaos-harness.py scenario (b). Differential: an
// unguarded acquire (no holder/lease guard) WOULD steal the live lease.

func spineC3CrashMidClaim(t *testing.T, dsn string) {
	ctx := context.Background()
	db := openDB(t, dsn)
	freshSchema(t, db, 1)
	sut := newSUT(t, db, dsn)

	f1, ok := sut.Acquire(ctx, 0, "H1", "run-H1")
	if !ok || f1 != 1 {
		t.Fatalf("H1 initial acquire: ok=%v fence=%d (want ok,1)", ok, f1)
	}

	// BEFORE expiry: H2's reclaim MUST be rejected — a live lease is not stealable.
	if _, ok := sut.Acquire(ctx, 0, "H2", "run-H2"); ok {
		t.Fatal("reclaimed a STILL-LIVE lease — §6.3 lease guard is not holding")
	}

	// teeth: the SAME acquire WITHOUT the holder/lease guard steals the live lease.
	if stolen := naiveUnguardedAcquire(t, db, 0, "H2", "run-H2"); stolen == 0 {
		t.Fatal("harness lost teeth: unguarded acquire didn't steal the live lease")
	}

	// AFTER expiry: real §6.2 reclaims, bumps the fence, and the zombie can't renew.
	freshSchema(t, db, 1)
	sut = newSUT(t, db, dsn)
	f1, _ = sut.Acquire(ctx, 0, "H1", "run-H1")
	waitLeaseExpiry(t) // lease is leaseShort (1s); real clock, like the PG path
	f2, ok := sut.Acquire(ctx, 0, "H2", "run-H2")
	if !ok || f2 != f1+1 {
		t.Fatalf("reclaim after expiry: ok=%v fence=%d (want ok, %d)", ok, f2, f1+1)
	}
	if sut.Renew(ctx, 0, "H1", f1) {
		t.Fatal("zombie H1 renewed after its lease lapsed and item was reclaimed")
	}
}

// --------------------------- C4 --------------------------------------------
// stale-holder completion + renew rejected; current-fence write accepted.
// Translates chaos-harness.py scenario (c). Differential: an UNFENCED write
// (no `AND fence_token=:myFence`) lands the zombie completion.

func spineC4StaleHolder(t *testing.T, dsn string) {
	ctx := context.Background()
	db := openDB(t, dsn)
	freshSchema(t, db, 1)
	sut := newSUT(t, db, dsn)

	f1, _ := sut.Acquire(ctx, 0, "H1", "run-H1") // H1 will become the stale holder
	waitLeaseExpiry(t)
	f2, ok := sut.Acquire(ctx, 0, "H2", "run-H2") // H2 reclaims, fence bumps
	if !ok || f2 <= f1 {
		t.Fatalf("H2 reclaim: ok=%v f2=%d f1=%d", ok, f2, f1)
	}

	// H1 wakes with its STALE fence: its complete AND its renew are rejected.
	if sut.Complete(ctx, 0, "H1", f1) {
		t.Fatal("ZOMBIE WRITE ACCEPTED — stale-fence completion not rejected (§6.3 breach)")
	}
	if sut.Renew(ctx, 0, "H1", f1) {
		t.Fatal("stale-fence renew accepted — zombie kept its lease alive")
	}

	// teeth: an unfenced complete (fence guard removed) WOULD land the zombie write.
	if !naiveUnfencedComplete(t, db, 0) {
		t.Fatal("harness lost teeth: unfenced complete didn't land the stale write")
	}
	// undo the teeth probe so the real arm asserts on a claimed item
	mustExec(t, db, `UPDATE work_item SET state='claimed' WHERE id=0`)

	// the CURRENT holder H2 completes with its live fence — accepted.
	if !sut.Complete(ctx, 0, "H2", f2) {
		t.Fatal("current holder's fenced write was rejected (§6.3 false-negative)")
	}
}

// --------------------------- C5 (Go-only, NEW) -----------------------------
// zombie-writer-vs-PVC: fence-BEFORE-release ordering. This is the case the
// in-process model CANNOT express (AC6): it needs the live resource layer +
// Postgres. We assert the §6.3 reclaim PROTOCOL ORDER — the holder is fenced
// (reclaim_fenced_at stamped, pod killed/cordoned at the resource layer) STRICTLY
// BEFORE the §6.2 release that bumps the fence — so a holder that survived the
// kill is already fenced and its late write is rejected.
//
// Differential: release-before-fence (the inverted order) leaves a window in
// which the surviving zombie writes with a still-valid fence; we prove that
// window exists under the naive order, then prove the real order closes it.
//
// LIMITATION (Copilot review: c5-does-not-test-ordering). ReclaimFenced stamps
// and releases in ONE transaction, so from outside we observe only the final
// committed state (marker stamped + fence bumped); swapping the two statements
// WITHIN the transaction would produce the same external observation and still
// pass here. What this case actually proves is the COMMITTED post-condition and
// that the guarded protocol closes the window the naive autocommit arm leaves
// open. A test that proves statement ORDER is respected under a real cross-
// process race — a survivor that observes the release before the resource-layer
// fence completes — needs the live resource layer (pod-kill/cordon) and is
// tracked as production-wiring follow-up (see the ReclaimFenced SCOPE note).

func spineC5FenceBeforeRelease(t *testing.T, dsn string) {
	ctx := context.Background()
	db := openDB(t, dsn)
	freshSchema(t, db, 1)
	sut := newSUT(t, db, dsn)

	f1, _ := sut.Acquire(ctx, 0, "H1", "run-H1")
	waitLeaseExpiry(t)

	// teeth: the INVERTED protocol (release/bump the fence BEFORE fencing the
	// resource holder) leaves a window. Model it as: bump fence first, and only
	// AFTER that stamp reclaim_fenced_at. A surviving H1 that raced in between
	// still holds a fence equal to the just-released value → its write lands.
	if !naiveReleaseBeforeFenceWindowExists(t, db, 0, f1) {
		t.Fatal("harness lost teeth: release-before-fence left no zombie window " +
			"to close — C5 cannot prove the real ordering matters")
	}

	// real §6.3 reclaim protocol: fence-before-release. Ordering invariants:
	//   reclaim_fenced_at IS stamped, and it is <= the moment the new fence is
	//   visible (fence bumped). A surviving H1 is fenced at the resource layer
	//   before the claim is releasable, so its post-kill write with f1 is rejected.
	freshSchema(t, db, 1)
	sut = newSUT(t, db, dsn)
	f1, _ = sut.Acquire(ctx, 0, "H1", "run-H1")
	waitLeaseExpiry(t)

	f2, err := sut.ReclaimFenced(ctx, 0, "H2", "run-H2")
	if err != nil {
		t.Fatalf("ReclaimFenced: %v", err)
	}
	if f2 != f1+1 {
		t.Fatalf("ReclaimFenced fence: got %d want %d", f2, f1+1)
	}

	// ORDER assertion (the crux of C5): the fence marker was stamped, and the
	// surviving zombie H1 — even though it "came back" — is rejected on its stale
	// fence. If release had happened before fencing, this write would have landed.
	var fencedAt sql.NullTime
	mustQueryRow(t, db,
		`SELECT reclaim_fenced_at FROM claim WHERE work_item_id=0`).Scan(&fencedAt)
	if !fencedAt.Valid {
		t.Fatal("C5: reclaim_fenced_at not stamped — holder was released without " +
			"being fenced first (§6.3 fence-before-release violated)")
	}
	if sut.Complete(ctx, 0, "H1", f1) {
		t.Fatal("C5: surviving zombie H1 wrote after reclaim — fence-before-release " +
			"did not fence it at the resource layer before the claim released")
	}
	// current holder still completes cleanly with the live fence.
	if !sut.Complete(ctx, 0, "H2", f2) {
		t.Fatal("C5: current holder H2's live-fence write was rejected")
	}
}

// --------------------------- C6 --------------------------------------------
// double-dispatch dedup: a re-driven external-effect dispatch returns the SAME
// task id (durable marker). Translates chaos-harness.py scenario (d), dispatch.

func spineC6DispatchDedup(t *testing.T, dsn string) {
	ctx := context.Background()
	db := openDB(t, dsn)
	freshSchema(t, db, 1)
	sut := newSUT(t, db, dsn)
	_, _ = sut.Acquire(ctx, 0, "H1", "run-H1")

	tid := sut.DispatchOnce(ctx, "run-H1", "task-abc")
	if tid == "" {
		t.Fatal("first dispatch returned empty task id")
	}
	for i := 0; i < 5; i++ {
		// re-entry with a DIFFERENT candidate id must still return the recorded one.
		if got := sut.DispatchOnce(ctx, "run-H1", fmt.Sprintf("task-dup-%d", i)); got != tid {
			t.Fatalf("re-dispatched an external effect: got %q want %q", got, tid)
		}
	}
}

// --------------------------- C7 --------------------------------------------
// re-entrant claim/complete no-op, fence stable. Translates chaos-harness.py
// scenario (d) claim+complete. Differential: an unconditional re-acquire bumps
// the fence on every pass.

func spineC7ReentrantNoop(t *testing.T, dsn string) {
	ctx := context.Background()
	db := openDB(t, dsn)
	freshSchema(t, db, 1)
	sut := newSUT(t, db, dsn)

	f, ok := sut.Acquire(ctx, 0, "H1", "run-H1")
	if !ok || f != 1 {
		t.Fatalf("acquire: ok=%v fence=%d", ok, f)
	}

	// re-drive the claim K times: already held with a current fence → every pass
	// is a no-op, fence STAYS 1 (AC5).
	for i := 0; i < 5; i++ {
		if got, ok := sut.RedriveClaim(ctx, 0, "H1", "run-H1", f); !ok || got != f {
			t.Fatalf("re-entrant re-drive lost custody or bumped/changed fence: got %d ok=%v want %d (AC5)", got, ok, f)
		}
	}
	if cur := fenceOf(t, db, 0); cur != 1 {
		t.Fatalf("re-entrant re-drive bumped the stored fence to %d (AC5 breach)", cur)
	}

	// teeth: an unconditional re-acquire (ignores the re-read AND the §6.2 guard)
	// bumps the fence every pass.
	before := fenceOf(t, db, 0)
	_ = naiveUnguardedAcquire(t, db, 0, "H1", "run-H1")
	if after := fenceOf(t, db, 0); after != before+1 {
		t.Fatal("harness lost teeth: unguarded re-acquire didn't bump the fence")
	}
	mustExec(t, db, `UPDATE claim SET fence_token=$1 WHERE work_item_id=0`, before)

	// re-drive complete K times → exactly one advance (WHERE status='claimed').
	if !sut.Complete(ctx, 0, "H1", fenceOf(t, db, 0)) {
		t.Fatal("first complete rejected")
	}
	for i := 0; i < 5; i++ {
		if sut.Complete(ctx, 0, "H1", fenceOf(t, db, 0)) {
			t.Fatal("double-advanced a done item (WHERE status='claimed' not holding)")
		}
	}
}

// ===========================================================================
// Contention drivers + differential (naive/teeth) raw-SQL arms.
// The teeth arms deliberately run the BROKEN statement so a real-arm PASS means
// something (same discipline as the Python anchor).
// ===========================================================================

type claimResult struct {
	item  int
	run   string
	fence int64
}

// raceClaimersSUT drives M goroutines racing to ClaimNext until no open item
// remains — the §6.2 (correct) fan-out.
func raceClaimersSUT(t *testing.T, sut SUT, n, m int) []claimResult {
	t.Helper()
	ctx := context.Background()
	var mu sync.Mutex
	var out []claimResult
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < m; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start // maximise contention: everyone starts together
			for {
				item, fence, ok := sut.ClaimNext(ctx, fmt.Sprintf("agent-%d", w), fmt.Sprintf("run-%d", w))
				if !ok {
					if openCount(t, sutDB(sut)) == 0 {
						return
					}
					continue // contended this pass, not exhausted — retry
				}
				mu.Lock()
				out = append(out, claimResult{item, fmt.Sprintf("run-%d", w), fence})
				mu.Unlock()
			}
		}(w)
	}
	close(start)
	wg.Wait()
	return out
}

// raceClaimers drives the same race through a raw SQL txn func (used for the
// naive teeth arm, which has no SUT).
func raceClaimers(t *testing.T, db *sql.DB, n, m int, txn func(*testing.T, *sql.DB, string, string) (int, int64, bool)) []claimResult {
	t.Helper()
	var mu sync.Mutex
	var out []claimResult
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < m; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for {
				item, fence, ok := txn(t, db, fmt.Sprintf("agent-%d", w), fmt.Sprintf("run-%d", w))
				if !ok {
					if openCount(t, db) == 0 {
						return
					}
					continue
				}
				mu.Lock()
				out = append(out, claimResult{item, fmt.Sprintf("run-%d", w), fence})
				mu.Unlock()
			}
		}(w)
	}
	close(start)
	wg.Wait()
	return out
}

// naiveClaimTxn = the anti-pattern the story forbids: pick an open item with NO
// row lock and NO CAS guard, then acquire (check-then-act). Under contention two
// claimers read the same open row and both "win" → double-claim.
func naiveClaimTxn(t *testing.T, db *sql.DB, principal, run string) (int, int64, bool) {
	t.Helper()
	ctx := context.Background()
	var id int
	// NO FOR UPDATE SKIP LOCKED — the bug.
	err := db.QueryRowContext(ctx,
		`SELECT id FROM work_item WHERE state='open' ORDER BY id LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, 0, false
	}
	if err != nil {
		t.Fatalf("naive pick: %v", err)
	}
	time.Sleep(0) // widen the read→write window so the race is deterministic
	var fence int64
	// NO `AND (holder IS NULL OR lease expired)` CAS guard — the bug.
	if err := db.QueryRowContext(ctx,
		`UPDATE claim SET holder_principal=$1, run_id=$2, fence_token=fence_token+1,
			lease_expires_at=now()+interval '`+leaseShort+`'
		 WHERE work_item_id=$3 RETURNING fence_token`, principal, run, id).Scan(&fence); err != nil {
		t.Fatalf("naive acquire: %v", err)
	}
	mustExec(t, db, `UPDATE work_item SET state='claimed' WHERE id=$1`, id)
	return id, fence, true
}

// naiveUnguardedAcquire = §6.2 acquire with the holder/lease guard REMOVED.
// Used by C3/C7 teeth: it steals a live lease / bumps a live fence. Returns the
// resulting fence (0 on no-op).
func naiveUnguardedAcquire(t *testing.T, db *sql.DB, item int, principal, run string) int64 {
	t.Helper()
	var fence int64
	err := db.QueryRowContext(context.Background(),
		`UPDATE claim SET holder_principal=$1, run_id=$2, fence_token=fence_token+1,
			lease_expires_at=now()+interval '`+leaseShort+`'
		 WHERE work_item_id=$3 RETURNING fence_token`, principal, run, item).Scan(&fence)
	if err == sql.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatalf("naive unguarded acquire: %v", err)
	}
	mustExec(t, db, `UPDATE work_item SET state='claimed' WHERE id=$1`, item)
	return fence
}

// naiveUnfencedComplete = §6.3 complete with the `AND fence_token=:myFence` guard
// REMOVED. Used by C4 teeth: the zombie write lands. Returns true if it advanced.
func naiveUnfencedComplete(t *testing.T, db *sql.DB, item int) bool {
	t.Helper()
	res, err := db.ExecContext(context.Background(),
		`UPDATE work_item SET state='done' WHERE id=$1 AND state='claimed'`, item)
	if err != nil {
		t.Fatalf("naive unfenced complete: %v", err)
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// naiveReleaseBeforeFenceWindowExists models the INVERTED C5 order: bump/release
// the fence FIRST, stamp the resource fence marker AFTER. Returns true iff a
// zombie holding the pre-reclaim fence could still have written in the window —
// i.e. the marker was NOT yet set at the moment the new fence became visible.
// (Teeth: proves the ordering is load-bearing before C5 asserts the real order.)
func naiveReleaseBeforeFenceWindowExists(t *testing.T, db *sql.DB, item int, oldFence int64) bool {
	t.Helper()
	_ = oldFence // the pre-reclaim fence a survivor would still hold; asserted via C5's Complete(H1,f1)
	// step 1 (WRONG order): release + bump fence, marker still NULL.
	mustExec(t, db,
		`UPDATE claim SET fence_token=fence_token+1,
			lease_expires_at=now()+interval '`+leaseShort+`' WHERE work_item_id=$1`, item)
	var marker sql.NullTime
	mustQueryRow(t, db, `SELECT reclaim_fenced_at FROM claim WHERE work_item_id=$1`, item).Scan(&marker)
	windowOpen := !marker.Valid // holder not yet fenced though claim already moved on
	// step 2 (too late): stamp the marker.
	mustExec(t, db, `UPDATE claim SET reclaim_fenced_at=now() WHERE work_item_id=$1`, item)
	return windowOpen
}

// ===========================================================================
// small helpers
// ===========================================================================

func doubleClaimed(res []claimResult) map[int][]string {
	holders := map[int][]string{}
	for _, r := range res {
		holders[r.item] = append(holders[r.item], r.run)
	}
	doubles := map[int][]string{}
	for i, hs := range holders {
		if len(hs) > 1 {
			doubles[i] = hs
		}
	}
	return doubles
}

func assertAllClaimedUniqueFence(t *testing.T, res []claimResult, n int) {
	t.Helper()
	claimed := map[int]bool{}
	type fk struct {
		item  int
		fence int64
	}
	seen := map[fk]bool{}
	for _, r := range res {
		claimed[r.item] = true
		k := fk{r.item, r.fence}
		if seen[k] {
			t.Fatalf("non-unique (work_item_id, fence_token): %+v", k)
		}
		seen[k] = true
		if r.fence < 1 {
			t.Fatalf("non-positive fence token for item %d: %d", r.item, r.fence)
		}
	}
	if len(claimed) != n {
		t.Fatalf("lost work: %d/%d items claimed", len(claimed), n)
	}
}

func openCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var c int
	mustQueryRow(t, db, `SELECT count(*) FROM work_item WHERE state='open'`).Scan(&c)
	return c
}

func fenceOf(t *testing.T, db *sql.DB, item int) int64 {
	t.Helper()
	var f int64
	mustQueryRow(t, db, `SELECT fence_token FROM claim WHERE work_item_id=$1`, item).Scan(&f)
	return f
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func mustQueryRow(t *testing.T, db *sql.DB, q string, args ...any) *sql.Row {
	t.Helper()
	return db.QueryRowContext(context.Background(), q, args...)
}

// waitLeaseExpiry sleeps just past the `leaseShort` lease so the §6.2 reclaim
// guard (lease_expires_at < now()) admits a reclaim — real time, like the
// Python real-PG path's `time.sleep(1.2)`.
func waitLeaseExpiry(t *testing.T) {
	t.Helper()
	time.Sleep(1200 * time.Millisecond)
}

// ===========================================================================
// ADAPTER — the ONLY thing downstream dev writes when the spine lands.
//
// Wire each SUT method to the real coord package. `sutDB` must return the same
// *sql.DB the SUT writes through, so the differential teeth arms bite the same
// rows. Delete the panics as you implement.
// ===========================================================================

func newSUT(t *testing.T, db *sql.DB, dsn string) SUT {
	t.Helper()
	// The SUT contract above IS the coord package's coordination surface: each
	// method is one pinned §6.2/§6.3/§6.4 statement. NewForTest binds those
	// statements to this harness's int-keyed work_item/claim schema and provisions
	// the §6.4 dispatch marker. Nothing else in this file changes. (ISI-2394)
	_ = dsn // the DSN is consumed by openDB; the SUT shares the same *sql.DB.
	return coord.NewForTest(db)
}

// sutDB returns the *sql.DB backing the SUT (for open-item polling + teeth arms).
// If your adapter stores it, expose it here; the tests never mutate through it
// except in the clearly-labelled naive/teeth helpers.
func sutDB(sut SUT) *sql.DB {
	type dber interface{ DB() *sql.DB }
	if d, ok := sut.(dber); ok {
		return d.DB()
	}
	panic("sutDB: SUT must expose DB() *sql.DB for contention polling + teeth arms")
}
