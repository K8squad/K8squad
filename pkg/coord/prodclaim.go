// prodclaim.go — the §6.2 SKIP LOCKED claim BOUND TO THE PRODUCTION SCHEMA
// (Story 2.2 / ISI-2523, R10 CORE). Where the harness Coordinator (coord.go)
// proves the guard set against the self-contained int-keyed chaos schema, this
// file binds the CLAIM path to the checked-in Story 2.1 coord schema
// (db/migrations/0001_coord_schema.sql): uuid-keyed work items, board lanes
// (§13), pre-provisioned claim rows (exactly one per item, F3), and the §6.5
// audit log.
//
// Story 2.2 contract: N open items, M concurrent claimers → every item ends up
// held by EXACTLY ONE Run. One ClaimNext call is ONE transaction:
//
//		SELECT … FOR UPDATE OF <work_item> SKIP LOCKED LIMIT 1   — pop, skipping
//		                                                            rows other Runs hold
//		UPDATE claim … WHERE <free-or-expired guard>              — rewrite the
//		                                                            pre-provisioned
//	                                                           checkout row in place
//		UPDATE work_item SET state=<claimed lane>                 — board advance
//		INSERT INTO audit_log (…, 'claim_acquired', …)            — §6.5 provenance
//
// The four statements commit atomically: there is no observable moment where an
// item is half-claimed (checkout row rewritten but lane unchanged, or vice
// versa). SKIP LOCKED means a contended row is SKIPPED, never lost — the loser
// retries and takes the next item, so the fan-out neither double-claims nor
// strands work. There are NO application-level locks: single-claim is a property
// of the row lock + the conditional UPDATE guard, proven under contention by
// TestSpineProdClaim (P1/P2) in the spine-chaos gate.
//
// Renumbering note: the Story 2.1 reconciliation mapped the loose story wording
// onto the §6.1 authoritative names — checkouts ≡ coord.claim, and the claim
// row is PRE-PROVISIONED per item by the work_item insert trigger, so "insert
// checkouts" is executed as an in-place rewrite of the single, structurally
// guaranteed claim row (two live leases on one item are impossible by PK, F3).
// "state=claimed" is the board advance todo → in_progress (§13 lanes; there is
// no 'claimed' lane — custody IS the claim row, the lane records progress).
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ProdConfig parameterises the production §6.2 claim. The table names are
// deliberately NOT configurable: they are pinned by db/migrations/0001_coord_schema.sql
// (coord.work_item / coord.claim / coord.audit_log), and parameterising them
// would invite drift from the shipped schema. Only the lease length and the
// board-lane values are bound here so tests can run a short lease.
type ProdConfig struct {
	// LeaseInterval is the SQL interval literal a fresh claim's lease is set
	// to on acquire, e.g. "30 seconds". It is a trusted, code-supplied literal
	// interpolated into the (otherwise fully bound-parameter) statement text —
	// same discipline as Config.LeaseInterval in coord.go.
	LeaseInterval string

	// ClaimableState is the board lane an item must sit in to be poppable
	// ("todo"). Backlog is parked/unscheduled and is NOT claimable (§13).
	ClaimableState string

	// ClaimedState is the lane the item advances to, in the SAME transaction,
	// when its checkout row is acquired ("in_progress" — the run now owns it).
	ClaimedState string
}

// DefaultProdConfig returns the production binding: a 30-second lease, pops
// from the todo lane, advances to in_progress (the §13 board lanes, the same
// transition a checkout performs).
func DefaultProdConfig() ProdConfig {
	return ProdConfig{
		LeaseInterval:  "30 seconds",
		ClaimableState: "todo",
		ClaimedState:   "in_progress",
	}
}

// ProdClaimer executes the production §6.2 SKIP LOCKED claim against the
// shipped coord schema. It holds no mutable Go state beyond the *sql.DB and
// its pinned statements, so it is safe for concurrent use by many goroutines
// (each claim opens its own transaction).
type ProdClaimer struct {
	db    *sql.DB
	cfg   ProdConfig
	pick  string
	acq   string
	mark  string
	audit string
}

// NewProdClaimer binds the production claim statements. It rejects an
// incomplete config up front (empty lease or lane values would otherwise
// surface as confusing SQL errors — or worse, silently unguarded statements —
// at claim time).
func NewProdClaimer(db *sql.DB, cfg ProdConfig) (*ProdClaimer, error) {
	if db == nil {
		return nil, errors.New("coord.NewProdClaimer: nil db")
	}
	if cfg.LeaseInterval == "" || cfg.ClaimableState == "" || cfg.ClaimedState == "" {
		return nil, fmt.Errorf("coord.NewProdClaimer: incomplete config %+v "+
			"(LeaseInterval, ClaimableState and ClaimedState are all required)", cfg)
	}
	return &ProdClaimer{
		db:  db,
		cfg: cfg,
		// (1) POP — the §6.2 fan-out statement. FOR UPDATE OF w takes the
		// work_item row lock (the contention point); SKIP LOCKED makes a
		// contended row invisible to this pass instead of blocking, so M
		// claimers fan out onto M DISTINCT items and the loser of a row never
		// waits — it takes the next one. The claim-row guard in the WHERE
		// (holder NULL or lease expired) keeps a popped item from having a
		// live foreign lease; the deterministic ORDER BY keeps the pop FIFO
		// (created_at, then id as the stable tiebreak).
		pick: `
			SELECT w.id
			  FROM coord.work_item w
			  JOIN coord.claim c ON c.work_item_id = w.id
			 WHERE w.state = $1
			   AND (c.holder_principal IS NULL OR c.lease_expires_at < clock_timestamp())
			 ORDER BY w.created_at, w.id
			 LIMIT 1
			 FOR UPDATE OF w SKIP LOCKED`,
		// (2) ACQUIRE — rewrite the pre-provisioned checkout row in place
		// under the §6.2 guard (holder IS NULL OR lease expired). fence_token
		// is bumped monotonically (never reset, never reused — §6.1); the
		// lease is stamped with clock_timestamp() (the LIVE wall clock, not
		// the transaction-frozen now()) so a statement that waited on the row
		// lock cannot commit an already-expired lease. Under READ COMMITTED a
		// concurrent writer that wins this row first makes the loser's UPDATE
		// re-evaluate the guard and match 0 rows → not acquired, rollback.
		acq: fmt.Sprintf(`
			UPDATE coord.claim
			   SET holder_principal = $1,
			       run_id = $2::uuid,
			       fence_token = fence_token + 1,
			       lease_expires_at = clock_timestamp() + interval '%s',
			       acquired_at = clock_timestamp(),
			       renewed_at = NULL,
			       initiated_by_user_id = $3::uuid
			 WHERE work_item_id = $4::uuid
			   AND (holder_principal IS NULL OR lease_expires_at < clock_timestamp())
			 RETURNING fence_token`, cfg.LeaseInterval),
		// (3) BOARD ADVANCE — only from the claimable lane. A concurrent lane
		// change makes this match 0 rows, which rolls back the WHOLE claim
		// (acquire included): the checkout and the lane move are one fact.
		mark: `
			UPDATE coord.work_item
			   SET state = $1
			 WHERE id = $2::uuid AND state = $3`,
		// (4) AUDIT — the §6.5 coordination-audit record of the checkout,
		// appended in the SAME transaction so an audited claim that did not
		// commit cannot exist.
		audit: `
			INSERT INTO coord.audit_log
			       (work_item_id, run_id, event_type, principal,
			        initiated_by_user_id, fence_token, from_state, to_state)
			VALUES ($1::uuid, $2::uuid, 'claim_acquired', $3,
			        $4::uuid, $5, $6, $7)`,
	}, nil
}

// ClaimNext pops and claims ONE claimable item in a single transaction:
// SKIP-LOCKED pop → guarded in-place checkout-row acquire (fence bump + lease)
// → board advance to the claimed lane → append-only claim_acquired audit row.
//
// Semantics of the return values:
//
//   - ok=true, err=nil: the item is ours — its checkout row now names
//     (principal, runID) at the returned (fresh, monotonic) fence, its lane has
//     advanced, and the audit row is committed with it.
//   - ok=false, err=nil: nothing claimable was available THIS pass — either the
//     lane is exhausted or every candidate row was contended (held by another
//     transaction's lock, or guarded by a live foreign lease). The caller
//     retries; SKIP LOCKED skips, it never loses work.
//   - err!=nil: infrastructure failure (dead connection, invalid uuid, …) —
//     the transaction was rolled back; no state changed for this attempt.
//
// principal and runID must be non-empty (a checkout is always held by a
// principal on behalf of a Run); initiatedByUserID may be empty (recorded as
// NULL — the §12.4 control-plane stamp is wired by the apiserver path).
func (p *ProdClaimer) ClaimNext(ctx context.Context, principal, runID, initiatedByUserID string) (itemID string, fence int64, ok bool, err error) {
	if principal == "" || runID == "" {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.ClaimNext: principal and runID are required (got principal=%q runID=%q)", principal, runID)
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.ClaimNext: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// (1) pop — SKIP LOCKED: contended rows are skipped, never waited on.
	var id string
	switch err := tx.QueryRowContext(ctx, p.pick, p.cfg.ClaimableState).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return "", 0, false, nil // exhausted or fully contended this pass
	case err != nil:
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.ClaimNext: pick: %w", err)
	}

	// (2) guarded acquire of the popped item's checkout row.
	var initiator any
	if initiatedByUserID != "" {
		initiator = initiatedByUserID
	}
	switch err := tx.QueryRowContext(ctx, p.acq, principal, runID, initiator, id).Scan(&fence); {
	case errors.Is(err, sql.ErrNoRows):
		// The guard rejected us (live foreign lease appeared between pick and
		// acquire). Roll back — nothing is claimed; the caller retries.
		return "", 0, false, nil
	case err != nil:
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.ClaimNext: acquire: %w", err)
	}

	// (3) board advance, atomic with the acquire above.
	res, err := tx.ExecContext(ctx, p.mark, p.cfg.ClaimedState, id, p.cfg.ClaimableState)
	if err != nil {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.ClaimNext: mark claimed: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// The item left the claimable lane concurrently (it was cancelled,
		// completed, …). Roll the whole claim back — checkout and lane move
		// are one transactional fact.
		return "", 0, false, nil
	}

	// (4) §6.5 audit, same transaction.
	if _, err := tx.ExecContext(ctx, p.audit, id, runID, principal, initiator, fence, p.cfg.ClaimableState, p.cfg.ClaimedState); err != nil {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.ClaimNext: audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.ClaimNext: commit: %w", err)
	}
	return id, fence, true, nil
}

// ClaimableCount reports how many items currently sit in the claimable lane.
// The concurrency harness uses it to tell "exhausted" (stop) from "contended"
// (retry); the apiserver run loop will use it the same way.
func (p *ProdClaimer) ClaimableCount(ctx context.Context) (int, error) {
	var n int
	if err := p.db.QueryRowContext(ctx,
		`SELECT count(*) FROM coord.work_item WHERE state = $1`,
		p.cfg.ClaimableState).Scan(&n); err != nil {
		return 0, fmt.Errorf("coord.ProdClaimer.ClaimableCount: %w", err)
	}
	return n, nil
}

// AcquireSpecific = §6.2 guarded acquire of a SPECIFIC item (the counterpart
// of the harness Coordinator.Acquire on the production schema). The Story 2.9
// coordinator dispatch (proddispatch.go) needs exactly this shape: the next
// worker claims the item the coordinator dispatched BY ID, not by popping the
// lane — and a zombie (a fenced or lapsed former holder, e.g. the completing
// run B trying to re-take an item worker C now holds) must be rejected by the
// same free-or-expired guard that makes ClaimNext single-claim.
//
// One transaction, same discipline as ClaimNext: guarded in-place checkout-row
// acquire (fence bump + lease) → board advance from the claimable lane → §6.5
// claim_acquired audit row. ok=false, err=nil means the guard rejected us (a
// live foreign lease, or the item left the claimable lane) — nothing changed.
func (p *ProdClaimer) AcquireSpecific(ctx context.Context, principal, runID, itemID, initiatedByUserID string) (string, int64, bool, error) {
	if principal == "" || runID == "" || itemID == "" {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.AcquireSpecific: principal, runID and itemID are required (got principal=%q runID=%q itemID=%q)", principal, runID, itemID)
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.AcquireSpecific: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var initiator any
	if initiatedByUserID != "" {
		initiator = initiatedByUserID
	}
	var fence int64
	switch err := tx.QueryRowContext(ctx, p.acq, principal, runID, initiator, itemID).Scan(&fence); {
	case errors.Is(err, sql.ErrNoRows):
		return "", 0, false, nil // live foreign lease (or no such item): guard rejected
	case err != nil:
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.AcquireSpecific: acquire: %w", err)
	}

	res, err := tx.ExecContext(ctx, p.mark, p.cfg.ClaimedState, itemID, p.cfg.ClaimableState)
	if err != nil {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.AcquireSpecific: mark claimed: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", 0, false, nil // left the claimable lane concurrently: roll back whole claim
	}

	if _, err := tx.ExecContext(ctx, p.audit, itemID, runID, principal, initiator, fence, p.cfg.ClaimableState, p.cfg.ClaimedState); err != nil {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.AcquireSpecific: audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.AcquireSpecific: commit: %w", err)
	}
	return itemID, fence, true, nil
}
