package coord

import (
	"context"
	"database/sql"
	"fmt"
)

// Spine is the coordination mechanism bound to a Postgres connection pool.
//
// It operates over the coordination-record tables (work_item / claim, plus the
// coord_dispatch outbox it provisions on demand). All fence-guarded writes go
// through the methods here; callers never issue the raw SQL themselves, which is
// what keeps "no application-level lock" and "every write is fence-guarded" a
// property of the code path rather than of caller discipline.
//
// The lease interval is always applied server-side as `now() + make_interval(secs
// => $N)` with leaseSeconds bound as a query PARAMETER — never interpolated into
// the SQL text — so every query below is a constant string with no injection
// surface.
type Spine struct {
	db           *sql.DB
	leaseSeconds float64
}

// defaultLeaseSeconds is the production lease window. Renewal (the heartbeat)
// runs well inside it; a holder that misses the window is reclaimable.
const defaultLeaseSeconds = 30

// New binds a Spine to db with the production lease window.
func New(db *sql.DB) *Spine {
	return &Spine{db: db, leaseSeconds: defaultLeaseSeconds}
}

// NewForTest binds a Spine to db with the short lease the chaos suite (ISI-2347,
// leaseShort = "1 second") expects, so its reclaim-after-expiry cases resolve on
// a real clock. This is the adapter target for the gate's newSUT.
func NewForTest(db *sql.DB) *Spine {
	return &Spine{db: db, leaseSeconds: 1}
}

// DB returns the underlying pool. The chaos gate's sutDB uses it for open-item
// polling and its differential teeth arms; those must bite the same rows this
// Spine writes through.
func (s *Spine) DB() *sql.DB { return s.db }

// ClaimNext is the §6.2 queue-pull: in ONE statement, pop the lowest open work
// item under a `FOR UPDATE SKIP LOCKED` row lock, run the conditional
// fence-bump acquire against its claim row, and flip the item to 'claimed'.
// The row lock is held for the whole statement, so two concurrent poppers never
// select the same row and no open row is ever lost (SKIP LOCKED skips a
// contended row, it does not drop it).
//
// Returns the claimed item, its fresh fence, and ok=false when no open+unlocked
// item was available on this pass.
func (s *Spine) ClaimNext(ctx context.Context, principal, run string) (item int, fence int64, ok bool) {
	const q = `
WITH picked AS (
    SELECT id FROM work_item
    WHERE state = 'open'
    ORDER BY id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
), acq AS (
    UPDATE claim SET
        holder_principal = $1,
        run_id           = $2,
        fence_token      = fence_token + 1,
        lease_expires_at = now() + make_interval(secs => $3::double precision)
    WHERE work_item_id IN (SELECT id FROM picked)
      AND (holder_principal IS NULL OR lease_expires_at < now())
    RETURNING work_item_id, fence_token
), done AS (
    UPDATE work_item SET state = 'claimed'
    WHERE id IN (SELECT work_item_id FROM acq)
)
SELECT work_item_id, fence_token FROM acq`
	err := s.db.QueryRowContext(ctx, q, principal, run, s.leaseSeconds).Scan(&item, &fence)
	if err != nil {
		// sql.ErrNoRows == nothing poppable this pass. NOTE: a transient DB/context
		// error also collapses to ok=false here because the SUT gate contract fixes
		// this signature (no error return); the gate never passes silently on an
		// outage (a failed post-condition query is a hard t.Fatal). Splitting a real
		// `error` out from expected contention/exhaustion is production-wiring
		// follow-up ISI-2399 (item 1), done at apiserver/operator wire-in.
		return 0, 0, false
	}
	return item, fence, true
}

// Acquire is the §6.2 targeted acquire of a SPECIFIC item — the shared primitive
// behind a queue-pull's acquire and a reclaim's re-acquire. It first locks the
// work item (`FOR UPDATE`, the SAME work-item-first lock order as ClaimNext, so
// the two can never deadlock) and validates it is NOT terminal: a `done` item is
// never re-openable, so a targeted acquire whose lease has since expired cannot
// resurrect completed work (the "done item never double-advances" invariant).
// Only then does the CAS guard (`holder IS NULL OR lease expired`) run: it admits
// an unheld or lease-lapsed item and rejects one under a live lease, and every
// admit bumps the monotonic fence. Returns the fresh fence and ok=false when the
// item is terminal or a live lease blocks.
func (s *Spine) Acquire(ctx context.Context, item int, principal, run string) (fence int64, ok bool) {
	const q = `
WITH locked AS (
    SELECT id FROM work_item
    WHERE id = $3
      AND state <> 'done'
    FOR UPDATE
), acq AS (
    UPDATE claim SET
        holder_principal = $1,
        run_id           = $2,
        fence_token      = fence_token + 1,
        lease_expires_at = now() + make_interval(secs => $4::double precision)
    WHERE work_item_id IN (SELECT id FROM locked)
      AND (holder_principal IS NULL OR lease_expires_at < now())
    RETURNING work_item_id, fence_token
), done AS (
    UPDATE work_item SET state = 'claimed'
    WHERE id IN (SELECT work_item_id FROM acq)
)
SELECT fence_token FROM acq`
	err := s.db.QueryRowContext(ctx, q, principal, run, item, s.leaseSeconds).Scan(&fence)
	if err != nil {
		// sql.ErrNoRows == the item is terminal or a live lease blocked us. As with
		// ClaimNext, a transient error also collapses to ok=false under the fixed
		// SUT contract signature; splitting a real `error` out is follow-up ISI-2399.
		return 0, false
	}
	return fence, true
}

// Renew is the §6.2 lease heartbeat: extend the lease only while we are STILL the
// live holder at our fence. The `fence_token = $3` term is the zombie
// discriminator — a holder that was reclaimed (its item's fence bumped) can no
// longer match, so its renew is a no-op. Returns true iff the lease was extended.
func (s *Spine) Renew(ctx context.Context, item int, principal string, fence int64) bool {
	const q = `
UPDATE claim SET lease_expires_at = now() + make_interval(secs => $4::double precision)
WHERE work_item_id     = $1
  AND holder_principal = $2
  AND fence_token      = $3
  AND lease_expires_at > now()`
	res, err := s.db.ExecContext(ctx, q, item, principal, fence, s.leaseSeconds)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// Complete is the §6.3 fenced terminal write: advance the item to 'done' only if
// the caller still holds it at its fence under a LIVE lease AND the item is still
// 'claimed'. The fence term rejects a stale (zombie) writer; the
// `lease_expires_at > now()` term (matching Renew) additionally rejects a holder
// whose lease has lapsed but has not yet been reclaimed — so the post-expiry,
// pre-reclaim window can no longer finalize work; and the `state = 'claimed'`
// term makes the advance idempotent — a re-driven complete on an already-done
// item matches zero rows, so it can never double-advance. Returns true iff it
// advanced.
func (s *Spine) Complete(ctx context.Context, item int, principal string, fence int64) bool {
	const q = `
UPDATE work_item SET state = 'done'
WHERE id = $1
  AND state = 'claimed'
  AND EXISTS (
      SELECT 1 FROM claim
      WHERE work_item_id     = $1
        AND holder_principal = $2
        AND fence_token      = $3
        AND lease_expires_at > now()
  )`
	res, err := s.db.ExecContext(ctx, q, item, principal, fence)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// RedriveClaim is the §6.4 reconcile-safe re-drive: on reconciliation we re-read
// the claim and, if we ALREADY hold it live at our fence, refresh the lease
// WITHOUT bumping the fence — an idempotent no-op that keeps a stable custody
// across arbitrarily many reconcile passes (AC5). Only when we are no longer the
// live holder does it fall through to a fresh conditional acquire. Returns the
// resulting fence (unchanged on the no-op path).
func (s *Spine) RedriveClaim(ctx context.Context, item int, principal, run string, fence int64) int64 {
	// The no-op fast path is taken ONLY when we still hold the claim live: same
	// holder, same run, same fence, AND an unexpired lease. Dropping any of these —
	// in particular a lapsed lease, or a DIFFERENT run reusing the same principal —
	// falls through to a real conditional re-acquire (which bumps the fence) rather
	// than silently refreshing a lease we no longer legitimately hold.
	const q = `
UPDATE claim SET lease_expires_at = now() + make_interval(secs => $4::double precision)
WHERE work_item_id     = $1
  AND holder_principal = $2
  AND fence_token      = $3
  AND run_id           = $5
  AND lease_expires_at > now()
RETURNING fence_token`
	var cur int64
	err := s.db.QueryRowContext(ctx, q, item, principal, fence, s.leaseSeconds, run).Scan(&cur)
	if err == nil {
		return cur // we still hold it live → no-op re-drive, fence stable
	}
	// Not the live holder at that fence: attempt a fresh conditional acquire.
	if f, ok := s.Acquire(ctx, item, principal, run); ok {
		return f
	}
	// A live foreign lease blocked the re-acquire: report the stored fence.
	return s.fenceOf(ctx, item)
}

// DispatchOnce is the §6.4 idempotent external-effect dispatch (the Story 2.5
// outbox). The FIRST dispatch for a run durably records its task id; every
// re-driven dispatch — even with a different candidate id — returns that same
// recorded id, so the side effect fires at most once. Returns "" only on error.
func (s *Spine) DispatchOnce(ctx context.Context, run, taskID string) string {
	if err := s.ensureOutbox(ctx); err != nil {
		return ""
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO coord_dispatch (run_id, task_id) VALUES ($1, $2)
ON CONFLICT (run_id) DO NOTHING`, run, taskID); err != nil {
		return ""
	}
	var got string
	if err := s.db.QueryRowContext(ctx,
		`SELECT task_id FROM coord_dispatch WHERE run_id = $1`, run).Scan(&got); err != nil {
		return ""
	}
	return got
}

// ReclaimFenced is the §6.3 crash-safe reclaim: FENCE-BEFORE-RELEASE. The order
// is load-bearing (C5). First stamp reclaim_fenced_at — the durable record that
// the previous holder has been fenced at the resource layer (pod killed /
// cordoned). ONLY THEN run the §6.2 conditional acquire that bumps the fence and
// installs the new holder. Because the survivor is fenced before the claim can be
// re-taken, a holder that outlived the kill already carries a stale fence and its
// late write is rejected — there is no window in which it can clobber.
//
// Both writes run in one transaction so a crash mid-protocol cannot leave the
// item fenced-but-unreleased or released-but-unfenced. Returns the bumped fence,
// or an error if the item is terminal or a still-live lease made it
// non-reclaimable.
//
// SCOPE (chaos gate): in this package "fence the holder" is the DURABLE
// reclaim_fenced_at stamp + fence bump, run strictly before the release, which
// proves the §6.3 SQL fence-before-release ORDER — a survivor's stale-fence
// write is rejected by Complete/Renew. It does NOT yet perform or confirm a
// resource-layer pod-kill/cordon, so a zombie could still issue direct
// PVC/resource writes. A confirmed, fail-closed resource-layer kill BEFORE
// release is tracked as production-wiring follow-up ISI-2399.
func (s *Spine) ReclaimFenced(ctx context.Context, item int, newPrincipal, newRun string) (fence int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	// 0. TERMINAL GUARD — lock the work item first (work-item-first order, same as
	// ClaimNext/Acquire) and refuse to reclaim a 'done' item. Without this, a
	// completed item whose retained lease has since expired could be reclaimed and
	// flipped 'done' -> 'claimed' below, defeating "a done item never double-advances".
	var state string
	err = tx.QueryRowContext(ctx,
		`SELECT state FROM work_item WHERE id = $1 FOR UPDATE`, item).Scan(&state)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("coord: ReclaimFenced: work_item %d does not exist", item)
	}
	if err != nil {
		return 0, err
	}
	if state == "done" {
		return 0, fmt.Errorf("coord: ReclaimFenced: work_item %d is terminal (done); refusing to reopen", item)
	}

	// 1. FENCE FIRST — mark the surviving holder fenced at the resource layer.
	if _, err = tx.ExecContext(ctx,
		`UPDATE claim SET reclaim_fenced_at = now() WHERE work_item_id = $1`, item); err != nil {
		return 0, err
	}

	// 2. RELEASE SECOND — the §6.2 conditional, fence-bumping acquire.
	const acq = `
UPDATE claim SET
    holder_principal = $1,
    run_id           = $2,
    fence_token      = fence_token + 1,
    lease_expires_at = now() + make_interval(secs => $4::double precision)
WHERE work_item_id = $3
  AND (holder_principal IS NULL OR lease_expires_at < now())
RETURNING fence_token`
	err = tx.QueryRowContext(ctx, acq, newPrincipal, newRun, item, s.leaseSeconds).Scan(&fence)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("coord: ReclaimFenced: work_item %d still holds a live lease", item)
	}
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE work_item SET state = 'claimed' WHERE id = $1`, item); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return fence, nil
}

// fenceOf reads the current stored fence for an item (0 if the row is absent).
func (s *Spine) fenceOf(ctx context.Context, item int) int64 {
	var f int64
	_ = s.db.QueryRowContext(ctx,
		`SELECT fence_token FROM claim WHERE work_item_id = $1`, item).Scan(&f)
	return f
}

// ensureOutbox provisions the durable dispatch-dedup marker table on demand. It
// is idempotent (CREATE … IF NOT EXISTS) so it is cheap to call before each
// dispatch and needs no separate migration step in the chaos fixture.
func (s *Spine) ensureOutbox(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS coord_dispatch (
    run_id     text        PRIMARY KEY,
    task_id    text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
)`)
	return err
}
