// Package coord is the Go coordination spine (Story 2.10 / ISI-2394): the
// claim / lease / fence / reclaim / dispatch mechanism that makes work-item
// custody safe under concurrency and crashes. It is the single most
// correctness-critical subsystem of v1 (PRD R10 / Arch §6).
//
// Every exported method here is ONE pinned SQL statement (or one short
// transaction) whose guard clauses are load-bearing:
//
//   - ClaimNext / Acquire  — §6.2 SKIP-LOCKED pop + conditional fence-bump acquire
//     (holder IS NULL OR lease expired). Guarantees at-most-one live holder and a
//     monotonic, never-reused fence_token.
//   - Renew                — §6.2 lease heartbeat gated on holder=me AND fence=mine
//     AND lease still live. A stale/zombie holder cannot keep its lease alive.
//   - Complete             — §6.3 fenced state-mutating write: the item advances
//     only under the CURRENT fence and only from the claimed state (no zombie
//     write, no double-advance).
//   - ReclaimFenced        — §6.3 fence-BEFORE-release reclaim: the dying holder is
//     fenced (reclaim_fenced_at stamped) strictly before the release that bumps
//     the fence, closing the survivor-write window.
//   - RedriveClaim         — §6.4 idempotent reconcile re-entry: re-driving a claim
//     we still hold is a no-op (no fence bump).
//   - DispatchOnce         — §6.4 idempotent external-effect dispatch guarded by a
//     durable marker AND a live-custody predicate (item+holder+run+fence under an
//     unexpired lease): a fenced/lapsed run cannot record the marker; re-entry by
//     the live holder returns the SAME recorded task id.
//
// All lease boundaries (assignment and guard) use clock_timestamp() — the live
// wall clock — never now() (frozen at statement start), so a statement that
// blocks on a row lock past the lease cannot commit an already-expired lease or
// evaluate a guard against a stale timestamp.
//
// The required chaos gate proves each guard bites: spine-chaos.yml runs
// TestSpine (C1..C7, ISI-2347) against a real Postgres with -race on, first
// executing the un-guarded ("naive") variant of each statement and asserting it
// breaks, then asserting the guarded statement here holds.
//
// # Schema binding
//
// The custody invariants are pure SQL patterns over two tables — work_item (the
// unit of coordination) and claim (exactly one row per item, holding the current
// holder + monotonic fence_token). The statement bodies below are parameterised
// only by a Config binding (table names, lease interval, lifecycle values), so
// the SAME guard SQL runs unchanged against any schema that supplies the columns
// they read/write.
//
// In THIS story (ISI-2394) the only schema they are bound to and proved against
// is the self-contained, int-keyed harness schema the chaos gate provisions (see
// NewForTest and spine_chaos_test.go's freshSchema): work_item(id int, state) and
// claim(work_item_id int, holder_principal, run_id, fence_token, lease_expires_at,
// reclaim_fenced_at), plus a run-keyed dispatch marker. That schema is the
// contract the C1..C7 guards are validated on.
//
// Production binding status: the §6.2 CLAIM path is bound to the checked-in
// production schema (db/migrations/0001_coord_schema.sql) by ProdClaimer
// (prodclaim.go, Story 2.2 / ISI-2523): uuid-keyed items, board lanes, the F3
// pre-provisioned claim rows, and the §6.5 audit row — proved on the shipped
// DDL by TestSpineProdClaim (P1/P2) in the same chaos gate. The remaining
// statements (Renew / Complete / ReclaimFenced / RedriveClaim / DispatchOnce)
// are still proved only on the int-keyed harness schema above: binding them to
// production additionally requires (b) adding reclaim_fenced_at + the §6.4
// dispatch marker to a forward migration, and (c) re-running the C3..C7 guards
// against that checked-in schema — the follow-up stories for reclaim/dispatch.
// Until then, treat everything except ProdClaimer as the chaos-gate-proven
// guard set, not the wired production coordinator. (Copilot review:
// schema-compat; production-wiring follow-up.)
package coord

import (
	"context"
	"database/sql"
	"fmt"
)

// Config binds the coordination statements to a concrete schema. All fields are
// trusted, code-supplied identifiers (never user input) — they are interpolated
// into statement text; every value travels as a bound parameter.
type Config struct {
	WorkItem string // (schema-qualified) work_item table name
	Claim    string // (schema-qualified) claim table name
	Dispatch string // (schema-qualified) §6.4 idempotency-marker table name

	// LeaseInterval is the SQL interval literal a claim's lease is (re)set to on
	// acquire/renew, e.g. "30 seconds". The chaos gate uses a short lease so the
	// reclaim-after-expiry cases (C3/C4/C5) run in real wall-clock time.
	LeaseInterval string

	// Work-item lifecycle values the spine reads/writes. Production maps these
	// onto the §6.1 board enum; the harness uses open/claimed/done.
	OpenState    string
	ClaimedState string
	DoneState    string
}

// Coordinator runs the coordination statements against a bound schema. It holds
// no mutable Go state beyond the *sql.DB and its Config, so its methods are safe
// for concurrent use by many goroutines (each opens its own transaction).
type Coordinator struct {
	db     *sql.DB
	cfg    Config
	fencer ResourceFencer
}

// ResourceFencer confirms a resource-layer fence of a dying claim holder BEFORE
// the coordinator releases that holder's claim to a new principal (§6.3). A
// production implementation cordons/kills the surviving process's pod (or revokes
// its resource-layer credential) and returns nil ONLY once that fence is
// CONFIRMED — so a zombie that outlived its lease can no longer issue direct
// PVC/resource writes outside the spine. A non-nil error means the fence could
// not be confirmed: ReclaimFenced then fails CLOSED (aborts the reclaim, leaves
// the claim with its original holder) rather than releasing to a new holder while
// the zombie may still be live. (Copilot review: reclaim-does-not-fence-resource-layer)
//
// Holder identifies the dying holder as recorded on the claim row at reclaim
// time: item is the work-item id, principal/run its holder_principal/run_id (both
// empty if the claim row carries no holder).
type ResourceFencer interface {
	Fence(ctx context.Context, item int, principal, run string) error
}

// New returns a Coordinator bound to cfg.
func New(db *sql.DB, cfg Config) *Coordinator { return &Coordinator{db: db, cfg: cfg} }

// WithFencer returns c wired to a resource-layer fencer whose confirmed success
// gates ReclaimFenced's release step (fail-closed). Passing nil (the default,
// unwired state) preserves the SQL-order-only reclaim the chaos gate proves: the
// durable reclaim_fenced_at stamp + fence bump, with no resource-layer kill. The
// Coordinator holds no other mutable state, so this returns the same *Coordinator
// for call chaining; wire it once at construction before concurrent use.
func (c *Coordinator) WithFencer(f ResourceFencer) *Coordinator { c.fencer = f; return c }

// DB exposes the backing handle (used by the chaos harness for open-item polling
// and its differential teeth arms).
func (c *Coordinator) DB() *sql.DB { return c.db }

// ClaimNext = §6.2 one-transaction SKIP-LOCKED pop → conditional fence-bump
// acquire → work_item = claimed. Returns the claimed item id, its fresh fence,
// and ok=false when no open+unlocked item was available this pass. (C1/C2)
func (c *Coordinator) ClaimNext(ctx context.Context, principal, run string) (int, int64, bool) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		panic(fmt.Errorf("coord.ClaimNext: begin: %w", err))
	}
	defer func() { _ = tx.Rollback() }()

	// Pop one open item under a row lock, skipping rows other Runs already hold.
	// SKIP LOCKED never blocks and never hands the same row to two Runs.
	var id int
	pick := fmt.Sprintf(
		`SELECT id FROM %s WHERE state=$1 ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1`,
		c.cfg.WorkItem)
	switch err := tx.QueryRowContext(ctx, pick, c.cfg.OpenState).Scan(&id); err {
	case nil:
		// claim it below
	case sql.ErrNoRows:
		return 0, 0, false // exhausted or fully contended this pass
	default:
		panic(fmt.Errorf("coord.ClaimNext: pick: %w", err))
	}

	fence, ok := c.acquireTx(ctx, tx, id, principal, run)
	if !ok {
		return 0, 0, false
	}
	if err := tx.Commit(); err != nil {
		panic(fmt.Errorf("coord.ClaimNext: commit: %w", err))
	}
	return id, fence, true
}

// Acquire = §6.2 conditional fence-bump acquire of a SPECIFIC item. Queue-pull
// and reclaim share the same guard: holder IS NULL OR lease expired. ok=false
// when a live lease blocks it. (C3)
func (c *Coordinator) Acquire(ctx context.Context, item int, principal, run string) (int64, bool) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		panic(fmt.Errorf("coord.Acquire: begin: %w", err))
	}
	defer func() { _ = tx.Rollback() }()

	fence, ok := c.acquireTx(ctx, tx, item, principal, run)
	if !ok {
		return 0, false
	}
	if err := tx.Commit(); err != nil {
		panic(fmt.Errorf("coord.Acquire: commit: %w", err))
	}
	return fence, true
}

// acquireTx is the shared §6.2 acquire body: bump the fence + install the holder
// only when the claim is free (unheld or lease-expired), then mark the item
// claimed. Returns ok=false (no fence returned) when a live lease blocks it.
func (c *Coordinator) acquireTx(ctx context.Context, tx *sql.Tx, item int, principal, run string) (int64, bool) {
	// Lease boundaries use clock_timestamp() (the live wall clock), NOT now()
	// (=transaction_timestamp, frozen at statement/transaction start). A statement
	// that BLOCKS on the FOR UPDATE / row lock past the lease window would, under
	// now(), still see the pre-wait instant — committing a lease that is already
	// expired (admitting an immediate second claimant) or reading an expired lease
	// as still-live in the guard. clock_timestamp() advances during the wait, so
	// both the expiry ASSIGNMENT and the `< clock_timestamp()` guard reflect real
	// time. (Copilot re-review: now-fixed-at-transaction-start)
	acquire := fmt.Sprintf(
		`UPDATE %s SET holder_principal=$1, run_id=$2,
			fence_token=fence_token+1, lease_expires_at=clock_timestamp()+interval '%s'
		 WHERE work_item_id=$3
		   AND (holder_principal IS NULL OR lease_expires_at < clock_timestamp())
		 RETURNING fence_token`,
		c.cfg.Claim, c.cfg.LeaseInterval)
	var fence int64
	switch err := tx.QueryRowContext(ctx, acquire, principal, run, item).Scan(&fence); err {
	case nil:
		// acquired
	case sql.ErrNoRows:
		return 0, false // live lease held by someone else
	default:
		panic(fmt.Errorf("coord.acquire: %w", err))
	}
	// §6.1 lifecycle guard: only a non-terminal item may be (re)marked claimed. A
	// done item must never be resurrected by a reclaim of its expired lease —
	// reject via a zero-row update so the whole transaction rolls back and the
	// fence bump above is undone. (Copilot review: acquire-reopens-done)
	mark := fmt.Sprintf(`UPDATE %s SET state=$1 WHERE id=$2 AND state <> $3`, c.cfg.WorkItem)
	res, err := tx.ExecContext(ctx, mark, c.cfg.ClaimedState, item, c.cfg.DoneState)
	if err != nil {
		panic(fmt.Errorf("coord.acquire: mark claimed: %w", err))
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, false // terminal (done) item: do not reopen
	}
	return fence, true
}

// Renew = §6.2 lease heartbeat: extend the lease only while we are still the
// recorded holder at our fence AND the lease is still live AND the work item is
// not terminal. A stale fence, a different holder, an already-lapsed lease, or a
// done item all reject. The terminal guard closes the §6.1 lifecycle gap where a
// completed item's claim row still carries a live principal+fence+lease, which
// would otherwise let a caller keep heartbeating a done item's lease forever.
// (Copilot review: renew-after-complete-keeps-lease) (C4)
func (c *Coordinator) Renew(ctx context.Context, item int, principal string, fence int64) bool {
	q := fmt.Sprintf(
		`UPDATE %s SET lease_expires_at=clock_timestamp()+interval '%s'
		 WHERE work_item_id=$1 AND holder_principal=$2 AND fence_token=$3
		   AND lease_expires_at > clock_timestamp()
		   AND EXISTS (SELECT 1 FROM %s WHERE id=$1 AND state <> $4)`,
		c.cfg.Claim, c.cfg.LeaseInterval, c.cfg.WorkItem)
	res, err := c.db.ExecContext(ctx, q, item, principal, fence, c.cfg.DoneState)
	if err != nil {
		panic(fmt.Errorf("coord.Renew: %w", err))
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// Complete = §6.3 fenced state-mutating write: the item advances to done only
// from the claimed state AND only under the caller's CURRENT fence+holder AND a
// still-LIVE lease. Rejects a stale-fence (zombie) write, a re-advance of an
// already-done item, and a holder whose lease lapsed while it waited on the
// work-item lock — the lease term is compared against clock_timestamp() (the
// live wall clock, not the frozen now()) so a lock wait that outlasts the lease
// cannot finalize on a stale timestamp. (Copilot re-review: complete-uses-
// frozen-now / post-expiry finalize window) (C4/C7)
func (c *Coordinator) Complete(ctx context.Context, item int, principal string, fence int64) bool {
	q := fmt.Sprintf(
		`UPDATE %s SET state=$1
		 WHERE id=$2 AND state=$3
		   AND EXISTS (SELECT 1 FROM %s
		               WHERE work_item_id=$2 AND holder_principal=$4 AND fence_token=$5
		                 AND lease_expires_at > clock_timestamp())`,
		c.cfg.WorkItem, c.cfg.Claim)
	res, err := c.db.ExecContext(ctx, q, c.cfg.DoneState, item, c.cfg.ClaimedState, principal, fence)
	if err != nil {
		panic(fmt.Errorf("coord.Complete: %w", err))
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// RedriveClaim = §6.4 reconcile-safe re-drive. If we still hold the claim LIVE
// (same principal AND same run AND our fence AND the lease has not lapsed),
// re-driving is a no-op and returns the SAME fence with ok=true (no bump).
// Otherwise (the fence advanced, the lease lapsed, or a different run) it
// re-acquires under the §6.2 guard, bumping the fence (ok=true). ok=false — with
// no fence returned — means a live FOREIGN lease blocks re-acquire: the caller
// does NOT hold custody and must not proceed with the returned value. (C7)
func (c *Coordinator) RedriveClaim(ctx context.Context, item int, principal, run string, fence int64) (int64, bool) {
	// Idempotent no-op ONLY when we still hold it LIVE. Checking run_id (not just
	// principal) and the un-lapsed lease is load-bearing: a lapsed lease or a
	// different run reusing the same principal must fall through to a real
	// re-acquire, not silently return the stale fence.
	// (Copilot review: redrive-fast-path-ignores-lease-and-run)
	q := fmt.Sprintf(
		`SELECT fence_token FROM %s
		 WHERE work_item_id=$1 AND holder_principal=$2 AND run_id=$3 AND fence_token=$4
		   AND lease_expires_at > clock_timestamp()`,
		c.cfg.Claim)
	var cur int64
	switch err := c.db.QueryRowContext(ctx, q, item, principal, run, fence).Scan(&cur); err {
	case nil:
		return cur, true // still ours, live — idempotent no-op, no bump
	case sql.ErrNoRows:
		// not live-ours → fall through to re-acquire
	default:
		panic(fmt.Errorf("coord.RedriveClaim: re-read: %w", err))
	}
	if f, ok := c.Acquire(ctx, item, principal, run); ok {
		return f, true
	}
	// A live foreign lease blocks re-acquire. Return an explicit blocked status
	// rather than another holder's fence, so the caller cannot mistake rejection
	// for custody. (Copilot review: redrive-returns-foreign-fence)
	return 0, false
}

// DispatchOnce is the §6.4 durable idempotency MARKER for external-effect
// dispatch — a get-or-create of the winning task id, keyed on run_id. The first
// call records taskID; every re-entry (even with a different candidate id)
// returns the SAME recorded id.
//
// It does NOT itself deliver the external effect. It provides the single stable
// id that a downstream dispatcher/outbox keys on so delivery is at-most-once per
// run: concurrent callers all observe the same winning id, so re-entry cannot
// fan out to distinct recorded ids, but the ACTUAL external call must be made
// idempotent on the returned id (or routed through such an outbox) by the caller.
// The outbox/delivery state machine that enforces exactly-once delivery is
// production-wiring follow-up, not part of this gate — hence the name reflects
// the marker's role (record-once), not a delivery guarantee this method makes.
// (Copilot review: dispatch-once-does-not-dispatch.) (C6)
//
// FENCE-GUARDED CREATION (Copilot re-review: dispatch-has-no-custody-predicate):
// marker creation is tied ATOMICALLY to a matching LIVE claim. The insert lands
// only when (item, principal, run, fence) is the current holder under an
// unexpired lease — the same custody predicate Complete/Renew enforce — so a
// fenced or lease-lapsed run can neither record the marker nor initiate the
// external effect it gates. Reading back an ALREADY-recorded id is not gated:
// once the legitimate live holder recorded it the effect is committed to fire
// once, and a later re-drive (even by a since-lapsed holder) may learn the id
// without re-firing. Returns "" when the caller holds no live fence-matching
// claim and no marker exists yet (a refused dispatch).
func (c *Coordinator) DispatchOnce(ctx context.Context, item int, principal, run string, fence int64, taskID string) string {
	// INSERT ... SELECT FROM claim WHERE <live custody>: no source row (hence no
	// insert) unless this caller is the current fence-matching holder under a live
	// lease. ON CONFLICT keeps first-writer-wins idempotency for concurrent re-drive.
	ins := fmt.Sprintf(
		`INSERT INTO %s(run_id, task_id)
		 SELECT $1, $2 FROM %s
		 WHERE work_item_id=$3 AND holder_principal=$4 AND run_id=$1 AND fence_token=$5
		   AND lease_expires_at > clock_timestamp()
		 ON CONFLICT (run_id) DO NOTHING`,
		c.cfg.Dispatch, c.cfg.Claim)
	if _, err := c.db.ExecContext(ctx, ins, run, taskID, item, principal, fence); err != nil {
		panic(fmt.Errorf("coord.DispatchOnce: insert: %w", err))
	}
	sel := fmt.Sprintf(`SELECT task_id FROM %s WHERE run_id=$1`, c.cfg.Dispatch)
	var got string
	switch err := c.db.QueryRowContext(ctx, sel, run).Scan(&got); err {
	case nil:
		return got
	case sql.ErrNoRows:
		return "" // refused: no live fence-matching claim and no prior marker
	default:
		panic(fmt.Errorf("coord.DispatchOnce: read-back: %w", err))
	}
}

// ReclaimFenced = §6.3 fence-BEFORE-release reclaim protocol. The dying holder is
// fenced first — reclaim_fenced_at is stamped (in the real cluster this pairs
// with a pod-kill/cordon at the resource layer) STRICTLY BEFORE the §6.2 release
// that bumps the fence and installs the new holder. Because the marker is stamped
// before the claim is releasable, a holder that survived the kill is already
// fenced and its late stale-fence write is rejected. Returns the bumped fence, or
// an error if the item still holds a live lease. (C5)
//
// RESOURCE-LAYER FENCE (Copilot review: reclaim-does-not-fence-resource-layer):
// when a ResourceFencer is wired (WithFencer), the "fence the holder" step is no
// longer the DB stamp alone: between the reclaim_fenced_at stamp and the release,
// the fencer must CONFIRM a resource-layer kill/cordon of the surviving process.
// If it cannot (Fence returns error), ReclaimFenced fails CLOSED — the whole
// transaction rolls back, so the release never becomes visible and the item keeps
// its original holder — rather than handing the claim to a new principal while a
// zombie may still issue direct PVC/resource writes. Because the fencer call sits
// strictly before the commit, an observer can never see the release ordered ahead
// of a confirmed fence. With no fencer wired (the default), behavior is unchanged:
// the SQL-order-only reclaim the chaos gate proves (durable stamp + fence bump),
// which fences a survivor against DB writes but not against out-of-band resource
// writes. The concrete K8s pod-kill/cordon fencer is supplied by the operator
// wiring that owns the live resource layer.
func (c *Coordinator) ReclaimFenced(ctx context.Context, item int, newPrincipal, newRun string) (int64, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("coord.ReclaimFenced: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// (1) fence the dying holder FIRST — durable DB marker.
	stamp := fmt.Sprintf(`UPDATE %s SET reclaim_fenced_at=clock_timestamp() WHERE work_item_id=$1`, c.cfg.Claim)
	if _, err := tx.ExecContext(ctx, stamp, item); err != nil {
		return 0, fmt.Errorf("coord.ReclaimFenced: stamp fence: %w", err)
	}
	// (1b) CONFIRM the resource-layer fence of the dying holder BEFORE any release.
	// The claim row still carries the outgoing holder here (the acquire below has
	// not run yet), so it names the exact process to fence. A confirm failure aborts
	// via the deferred rollback — fail-closed, no release, holder unchanged.
	if c.fencer != nil {
		var holder, holderRun sql.NullString
		who := fmt.Sprintf(`SELECT holder_principal, run_id FROM %s WHERE work_item_id=$1`, c.cfg.Claim)
		switch err := tx.QueryRowContext(ctx, who, item).Scan(&holder, &holderRun); err {
		case nil, sql.ErrNoRows:
			// proceed: empty holder → fence a no-op holder (fencer decides)
		default:
			return 0, fmt.Errorf("coord.ReclaimFenced: read holder: %w", err)
		}
		if err := c.fencer.Fence(ctx, item, holder.String, holderRun.String); err != nil {
			return 0, fmt.Errorf("coord.ReclaimFenced: resource-layer fence unconfirmed, refusing release: %w", err)
		}
	}
	// (2) THEN the §6.2 release+acquire that bumps the fence.
	fence, ok := c.acquireTx(ctx, tx, item, newPrincipal, newRun)
	if !ok {
		return 0, fmt.Errorf("coord.ReclaimFenced: item %d still holds a live lease", item)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("coord.ReclaimFenced: commit: %w", err)
	}
	return fence, nil
}

// NewForTest binds the coordination statements to the self-contained int-keyed
// schema the chaos harness provisions (public work_item/claim with states
// open/claimed/done and a short 1-second lease). It also (re)creates the §6.4
// dispatch idempotency-marker table, which the harness schema does not seed, so
// the marker starts empty for each freshly constructed SUT.
func NewForTest(db *sql.DB) *Coordinator {
	ctx := context.Background()
	for _, s := range []string{
		`DROP TABLE IF EXISTS dispatch`,
		`CREATE TABLE dispatch(run_id text PRIMARY KEY, task_id text NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			panic(fmt.Errorf("coord.NewForTest: provision dispatch marker: %w", err))
		}
	}
	return New(db, Config{
		WorkItem:      "work_item",
		Claim:         "claim",
		Dispatch:      "dispatch",
		LeaseInterval: "1 second",
		OpenState:     "open",
		ClaimedState:  "claimed",
		DoneState:     "done",
	})
}
