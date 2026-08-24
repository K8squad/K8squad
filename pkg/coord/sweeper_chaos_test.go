//go:build chaos

// sweeper_chaos_test.go — the Story 2.4 / ISI-3104 background reclaim-sweeper
// chaos gate (S1..S7), run by the same spine-chaos workflow as TestSpine (the
// workflow's -run 'TestSpine' filter matches TestSpineSweep unanchored). Every
// case runs against a REAL Postgres with -race on, because the properties under
// test ARE the durability properties the §6.2/§6.3 reclaim guards enforce:
//
//	S1 expired lease reclaimed         item → open, fence bumped, holder cleared,
//	                                   reclaim_fenced_at stamped; the stale holder's
//	                                   Complete/Renew are fenced out (AC 2.4)
//	S2 live lease NOT reclaimed        an unexpired lease is left alone (guard bites)
//	S3 done item NOT reopened          a done item's expired lease is not resurrected
//	S4 concurrent sweepers exactly-once N expired claims, two sweepers: every claim
//	                                   reclaimed once, fence bumped by exactly 1
//	S5 crash-safe re-derivation        a cancelled sweeper leaves the durable claim
//	                                   table intact; a fresh sweeper reclaims it
//	S6 clean context cancellation      Sweeper.Run returns context.Canceled promptly
//	S7 background loop reclaims         NewSweeper.Run reclaims on its own ticker and
//	                                   drives OnReclaim with the batch
package coord_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

// ===========================================================================
// TestSpineSweep — the reclaim-sweeper gate entrypoint.
// ===========================================================================
func TestSpineSweep(t *testing.T) {
	dsn := dsnOrFatal(t)

	t.Run("S1_expired_lease_reclaimed_and_fenced", func(t *testing.T) { sweepS1Reclaim(t, dsn) })
	t.Run("S2_live_lease_not_reclaimed", func(t *testing.T) { sweepS2LiveLease(t, dsn) })
	t.Run("S3_done_item_not_reopened", func(t *testing.T) { sweepS3DoneItem(t, dsn) })
	t.Run("S4_concurrent_sweepers_exactly_once", func(t *testing.T) { sweepS4ExactlyOnce(t, dsn) })
	t.Run("S5_crash_safe_rederivation", func(t *testing.T) { sweepS5CrashSafe(t, dsn) })
	t.Run("S6_clean_context_cancel", func(t *testing.T) { sweepS6Cancel(t, dsn) })
	t.Run("S7_background_loop_reclaims", func(t *testing.T) { sweepS7BackgroundLoop(t, dsn) })
}

// newSweepSUT provisions a fresh harness schema (n items) and a SweepStore bound
// to it, plus a Coordinator over the same tables to set up the held/expired state.
func newSweepSUT(t *testing.T, dsn string, n int) (*coord.SweepStore, *coord.Coordinator, *sql.DB) {
	t.Helper()
	db := openDB(t, dsn)
	freshSchema(t, db, n)
	s, err := coord.NewSweepForTest(db)
	if err != nil {
		t.Fatalf("NewSweepForTest: %v", err)
	}
	return s, coord.NewForTest(db), db
}

// sweepCollector is the OnReclaim sink shared by (possibly many) sweepers.
type sweepCollector struct {
	mu      sync.Mutex
	batches [][]coord.Reclaimed
}

func (c *sweepCollector) onReclaim(_ context.Context, r []coord.Reclaimed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batches = append(c.batches, r)
}

func (c *sweepCollector) items() (flat []coord.Reclaimed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.batches {
		flat = append(flat, b...)
	}
	return flat
}

func (c *sweepCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, b := range c.batches {
		n += len(b)
	}
	return n
}

// ---------------------------------------------------------------------------
// S1 — an expired lease is reclaimed: the item returns to open with a bumped
// fence, holder cleared and reclaim_fenced_at stamped, and the stale holder can
// no longer complete or renew (§6.2/§6.3 reclaim, Story 2.4 AC).
// ---------------------------------------------------------------------------
func sweepS1Reclaim(t *testing.T, dsn string) {
	s, coord0, db := newSweepSUT(t, dsn, 1)
	ctx := context.Background()

	f1, ok := coord0.Acquire(ctx, 0, "H1", "run-H1")
	if !ok {
		t.Fatal("setup: H1 acquire failed")
	}
	waitLeaseExpiry(t)

	reclaimed, err := s.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Item != 0 {
		t.Fatalf("reclaimed %v, want the single expired item 0", reclaimed)
	}
	if reclaimed[0].Fence != f1+1 {
		t.Fatalf("reclaimed fence = %d, want %d (bumped exactly once)", reclaimed[0].Fence, f1+1)
	}

	// Durable post-conditions: item open, claim released, fence bumped, stamped.
	var state string
	var holder sql.NullString
	var fence int64
	var fencedAt sql.NullTime
	var lease sql.NullTime
	mustQueryRow(t, db, `SELECT w.state, c.holder_principal, c.fence_token, c.reclaim_fenced_at, c.lease_expires_at
		FROM work_item w JOIN claim c ON c.work_item_id=w.id WHERE w.id=0`).
		Scan(&state, &holder, &fence, &fencedAt, &lease)
	if state != "open" {
		t.Fatalf("work_item.state = %q after reclaim, want open (returned to the pool)", state)
	}
	if holder.Valid {
		t.Fatalf("holder = %q after reclaim, want NULL (released)", holder.String)
	}
	if fence != f1+1 {
		t.Fatalf("fence_token = %d after reclaim, want %d", fence, f1+1)
	}
	if !fencedAt.Valid {
		t.Fatal("reclaim_fenced_at not stamped by the sweeper (§6.3 marker)")
	}
	if lease.Valid {
		t.Fatal("lease_expires_at not cleared after reclaim")
	}

	// The resurrected stale holder H1 (still at fence f1) cannot complete or renew.
	if coord0.Complete(ctx, 0, "H1", f1) {
		t.Fatal("stale holder H1 completed the reclaimed item — it was NOT fenced out")
	}
	if coord0.Renew(ctx, 0, "H1", f1) {
		t.Fatal("stale holder H1 renewed the reclaimed item's lease — it was NOT fenced out")
	}

	// The item is claimable again by another Run.
	f2, ok := coord0.Acquire(ctx, 0, "H2", "run-H2")
	if !ok {
		t.Fatal("reclaimed item was not claimable by H2")
	}
	if f2 <= f1 {
		t.Fatalf("H2 fence = %d, want > %d (monotonic)", f2, f1)
	}
}

// ---------------------------------------------------------------------------
// S2 — a LIVE lease is not reclaimed. The guard (lease_expires_at <
// clock_timestamp()) must leave an unexpired claim untouched.
// ---------------------------------------------------------------------------
func sweepS2LiveLease(t *testing.T, dsn string) {
	s, coord0, db := newSweepSUT(t, dsn, 1)
	ctx := context.Background()

	f1, ok := coord0.Acquire(ctx, 0, "H1", "run-H1")
	if !ok {
		t.Fatal("setup: acquire failed")
	}
	// Do NOT wait for expiry — the lease is live.

	reclaimed, err := s.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if len(reclaimed) != 0 {
		t.Fatalf("reclaimed %v while the lease was live — the guard did not bite", reclaimed)
	}

	var state string
	var holder sql.NullString
	var fence int64
	mustQueryRow(t, db, `SELECT w.state, c.holder_principal, c.fence_token
		FROM work_item w JOIN claim c ON c.work_item_id=w.id WHERE w.id=0`).
		Scan(&state, &holder, &fence)
	if state != "claimed" || holder.String != "H1" || fence != f1 {
		t.Fatalf("live claim disturbed: state=%q holder=%q fence=%d, want claimed/H1/%d",
			state, holder.String, fence, f1)
	}
}

// ---------------------------------------------------------------------------
// S3 — a DONE item's expired lease is not resurrected. Completing an item leaves
// its claim row with a live principal+fence; once its lease lapses the sweeper
// must NOT reopen it (the acquire-reopens-done guard, applied to reclaim).
// ---------------------------------------------------------------------------
func sweepS3DoneItem(t *testing.T, dsn string) {
	s, coord0, db := newSweepSUT(t, dsn, 1)
	ctx := context.Background()

	f1, ok := coord0.Acquire(ctx, 0, "H1", "run-H1")
	if !ok {
		t.Fatal("setup: acquire failed")
	}
	if !coord0.Complete(ctx, 0, "H1", f1) {
		t.Fatal("setup: complete failed")
	}
	waitLeaseExpiry(t) // the done item's claim row still carries H1's now-lapsed lease

	reclaimed, err := s.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if len(reclaimed) != 0 {
		t.Fatalf("reclaimed %v — a DONE item was reopened by the sweeper", reclaimed)
	}

	var state string
	var fence int64
	mustQueryRow(t, db, `SELECT w.state, c.fence_token
		FROM work_item w JOIN claim c ON c.work_item_id=w.id WHERE w.id=0`).Scan(&state, &fence)
	if state != "done" {
		t.Fatalf("done item state = %q after sweep, want done (never reopened)", state)
	}
	if fence != f1 {
		t.Fatalf("done item fence bumped to %d (want %d) — the sweeper touched a terminal item", fence, f1)
	}
}

// ---------------------------------------------------------------------------
// S4 — two concurrent sweepers, N expired claims: SKIP LOCKED partitions the due
// set; every claim is reclaimed EXACTLY once (fence bumped by exactly 1, no
// double-bump), none lost.
// ---------------------------------------------------------------------------
func sweepS4ExactlyOnce(t *testing.T, dsn string) {
	const n = 24
	s, coord0, db := newSweepSUT(t, dsn, n)
	ctx := context.Background()

	baseFence := make(map[int]int64, n)
	for i := 0; i < n; i++ {
		f, ok := coord0.Acquire(ctx, i, "H1", "run-H1")
		if !ok {
			t.Fatalf("setup: acquire item %d failed", i)
		}
		baseFence[i] = f
	}
	waitLeaseExpiry(t)

	// A second store over its own connection pool, so the two sweepers contend as
	// independent clients would.
	db2 := openDB(t, dsn)
	s2, err := coord.NewSweepForTest(db2)
	if err != nil {
		t.Fatalf("NewSweepForTest(2): %v", err)
	}

	shared := &sweepCollector{}
	var wg sync.WaitGroup
	// Each sweeper scans repeatedly until the whole due set is drained; SKIP LOCKED
	// makes the two partition the work with no overlap.
	drain := func(store *coord.SweepStore) {
		defer wg.Done()
		for shared.count() < n {
			batch, err := store.ReclaimExpired(ctx)
			if err != nil {
				t.Errorf("ReclaimExpired: %v", err)
				return
			}
			if len(batch) > 0 {
				shared.onReclaim(ctx, batch)
			}
		}
	}
	wg.Add(2)
	go drain(s)
	go drain(s2)
	wg.Wait()

	// Every item reclaimed exactly once.
	seen := make(map[int]int)
	for _, r := range shared.items() {
		seen[r.Item]++
		if r.Fence != baseFence[r.Item]+1 {
			t.Errorf("item %d reclaimed fence = %d, want %d (single bump)", r.Item, r.Fence, baseFence[r.Item]+1)
		}
	}
	for i := 0; i < n; i++ {
		if seen[i] != 1 {
			t.Errorf("item %d reclaimed %d times, want exactly 1", i, seen[i])
		}
	}
	if len(seen) != n {
		t.Fatalf("distinct reclaimed items = %d, want %d", len(seen), n)
	}

	// Durable agreement: all items open, all fences bumped by exactly 1, none held.
	var open, held int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM work_item WHERE state='open'`).Scan(&open); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM claim WHERE holder_principal IS NOT NULL`).Scan(&held); err != nil {
		t.Fatalf("count held: %v", err)
	}
	if open != n {
		t.Fatalf("open items = %d, want %d", open, n)
	}
	if held != 0 {
		t.Fatalf("held claims = %d, want 0 (all released)", held)
	}
}

// ---------------------------------------------------------------------------
// S5 — crash-safety: a sweeper cancelled before it scans leaves the durable claim
// table exactly as it was; a fresh sweeper (no in-memory handoff) re-derives the
// due set and reclaims it. The reclaim itself is exactly-once across the restart.
// ---------------------------------------------------------------------------
func sweepS5CrashSafe(t *testing.T, dsn string) {
	s, coord0, db := newSweepSUT(t, dsn, 1)
	ctx := context.Background()

	f1, ok := coord0.Acquire(ctx, 0, "H1", "run-H1")
	if !ok {
		t.Fatal("setup: acquire failed")
	}
	waitLeaseExpiry(t)

	// Sweeper A starts and is cancelled immediately — its ticker (50ms) has not
	// fired, so it reclaims nothing and dies with the expired claim still durable.
	cA := &sweepCollector{}
	swA := coord.NewSweeper(s, nil, cA.onReclaim)
	ctxA, cancelA := context.WithCancel(context.Background())
	errcA := make(chan error, 1)
	go func() { errcA <- swA.Run(ctxA) }()
	cancelA()
	<-errcA
	if cA.count() != 0 {
		t.Fatalf("sweeper A reclaimed %d before cancel, want 0", cA.count())
	}

	// The claim is untouched: still held by H1 at f1, item still claimed.
	var holder sql.NullString
	var fence int64
	mustQueryRow(t, db, `SELECT holder_principal, fence_token FROM claim WHERE work_item_id=0`).Scan(&holder, &fence)
	if holder.String != "H1" || fence != f1 {
		t.Fatalf("claim disturbed by the cancelled sweeper: holder=%q fence=%d, want H1/%d",
			holder.String, fence, f1)
	}

	// Sweeper B restarts against the SAME durable state and reclaims it.
	cB := &sweepCollector{}
	s2, err := coord.NewSweepForTest(openDB(t, dsn))
	if err != nil {
		t.Fatalf("NewSweepForTest(B): %v", err)
	}
	// Rebind s2 to the existing schema without wiping it (NewSweepForTest only
	// creates a store; it does not touch tables), so B reclaims what A left.
	swB := coord.NewSweeper(s2, nil, cB.onReclaim)
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	go func() { _ = swB.Run(ctxB) }()

	waitFor(t, 3*time.Second, func() bool { return cB.count() == 1 })
	got := cB.items()
	if len(got) != 1 || got[0].Item != 0 || got[0].Fence != f1+1 {
		t.Fatalf("sweeper B reclaimed %v, want item 0 at fence %d", got, f1+1)
	}

	// Exactly-once across the restart: a manual scan now finds nothing.
	rest, err := s.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("post-restart ReclaimExpired: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("post-restart scan reclaimed %v — not exactly-once", rest)
	}
}

// ---------------------------------------------------------------------------
// S6 — Sweeper.Run returns context.Canceled promptly when its context is
// cancelled, with no dangling goroutine (a real ticker would be stopped by the
// deferred stop).
// ---------------------------------------------------------------------------
func sweepS6Cancel(t *testing.T, dsn string) {
	s, _, _ := newSweepSUT(t, dsn, 0)
	sw := coord.NewSweeper(s, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- sw.Run(ctx) }()
	cancel()

	select {
	case err := <-errc:
		if err != context.Canceled {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// ---------------------------------------------------------------------------
// S7 — the background loop reclaims on its own ticker: an expired claim is
// reclaimed by NewSweeper.Run with no manual ReclaimExpired call, and OnReclaim
// fires with the batch.
// ---------------------------------------------------------------------------
func sweepS7BackgroundLoop(t *testing.T, dsn string) {
	s, coord0, db := newSweepSUT(t, dsn, 1)
	ctx := context.Background()

	f1, ok := coord0.Acquire(ctx, 0, "H1", "run-H1")
	if !ok {
		t.Fatal("setup: acquire failed")
	}
	waitLeaseExpiry(t)

	c := &sweepCollector{}
	sw := coord.NewSweeper(s, nil, c.onReclaim) // 50ms interval from NewSweepForTest
	rctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- sw.Run(rctx) }()

	waitFor(t, 3*time.Second, func() bool { return c.count() == 1 })
	got := c.items()
	if len(got) != 1 || got[0].Item != 0 || got[0].Fence != f1+1 {
		t.Fatalf("background loop reclaimed %v, want item 0 at fence %d", got, f1+1)
	}

	var state string
	mustQueryRow(t, db, `SELECT state FROM work_item WHERE id=0`).Scan(&state)
	if state != "open" {
		t.Fatalf("state = %q after background reclaim, want open", state)
	}

	cancel()
	if err := <-errc; err != context.Canceled {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

// waitFor polls a condition on the test side only (the SUT's loop is what drives
// the state; the poll just observes it).
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
