// prodreconcile.go — the crash-safe Run reconcile machine's durable Store BOUND
// TO THE PRODUCTION SCHEMA (Story 3.1 / ISI-2655, R10 CORE). Where pkg/reconcile
// (ISI-2535) proves the state-machine logic over a small Store/Effects seam and
// MemStore is the in-memory model its falsification drives, this file is the
// physical §6.4 Store: it binds reconcile.Store to the checked-in coord schema
// (db/migrations 0001 + 0003) — uuid-keyed claim rows carrying reconcile_step +
// fence_token, the §6.5 audit log, and the §6.6 outbox.
//
// The load-bearing method is Advance — the conditional step-advance that IS the
// reconcile machine's serialization point (arch §6.4, AC3/AC4/AC6). One Advance is
// ONE transaction co-committing three facts that must never diverge:
//
//	UPDATE coord.claim SET reconcile_step = :next                      — the durable step
//	 WHERE work_item_id = :item AND reconcile_step = :expected         — step-CAS (same-fence competitor, AC4)
//	   AND (:fence IS NULL OR fence_token = :fence)                    — fence equality (reclaim zombie, AC4)
//	INSERT INTO coord.audit_log (…, 'reconcile_advanced', …)          — §6.5 provenance
//	INSERT INTO coord.outbox    (…, from_step, to_step, …)            — §6.6 exactly-once projection
//
// If the guarded UPDATE matches 0 rows (a stale/duplicate/zombie pass whose
// expected step or fence no longer holds) the whole transaction rolls back and
// Advance reports false — no double-advance, no audit/outbox row, no resurrection.
// If it matches its one row, the audit + outbox rows commit WITH it: there is no
// observable moment where the step advanced but its provenance/projection did not
// (AC6). A nil fence models the NAIVE reconciler (no fence guard) the falsification
// flips to prove fencing is load-bearing; production always passes a fence.
//
// Error seam. reconcile.Store's methods return no error (the machine is
// level-triggered and treats a false Advance as "did not commit, retry next
// pass"). A ProdReconcileStore therefore CAPTURES the first infrastructure error
// in a field and returns the safe zero value (Advance/Reclaim → false, Step → "",
// Fence → 0). The reconcile-loop caller (the CRD-status controller) MUST check
// Err() after a drive and requeue on a non-nil error rather than treat a stalled
// step as terminal. This mirrors the existing prod binding discipline (a rolled-
// back transaction changed nothing) without forcing the seam to grow error returns.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/K8squad/K8squad/pkg/reconcile"
)

// ProdReconcileStore implements reconcile.Store against the production coord
// schema for ONE Run's claim row (keyed by work_item_id). It is constructed per
// Run by the controller's reconcile loop; run_id and principal are bound here (the
// Store interface carries neither) and stamped onto every co-committed audit/outbox
// row. It holds no mutable state beyond the *sql.DB, its bound identifiers, and the
// captured error — a fresh Store reconstructs the Run's position from Postgres
// alone (AC2), so it is safe to discard and rebuild on every reconcile pass.
type ProdReconcileStore struct {
	db          *sql.DB
	ctx         context.Context
	workItemID  string // uuid — the claim row this Store drives
	runID       string // uuid — stamped onto audit_log.run_id / outbox.run_id
	principal   string // who is driving (audit_log.principal, §6.5)
	initiatedBy string // §12.4 control-plane stamp (may be empty → NULL)

	err error // first infrastructure error; sticky (see the error-seam note above)
}

// NewProdReconcileStore binds a durable Store to one Run's claim row. workItemID,
// runID and principal are required (an advance is always audited as some principal
// driving some Run over some item); initiatedByUserID may be empty (recorded NULL —
// the §12.4 stamp is wired by the apiserver path). ctx bounds every statement.
func NewProdReconcileStore(ctx context.Context, db *sql.DB, workItemID, runID, principal, initiatedByUserID string) (*ProdReconcileStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewProdReconcileStore: nil db")
	}
	if workItemID == "" || runID == "" || principal == "" {
		return nil, fmt.Errorf("coord.NewProdReconcileStore: workItemID, runID and principal are required "+
			"(got workItemID=%q runID=%q principal=%q)", workItemID, runID, principal)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &ProdReconcileStore{
		db:          db,
		ctx:         ctx,
		workItemID:  workItemID,
		runID:       runID,
		principal:   principal,
		initiatedBy: initiatedByUserID,
	}, nil
}

// Err returns the first infrastructure error captured by any method, or nil. The
// reconcile-loop caller checks it after a drive and requeues on non-nil rather
// than reading a stalled step as terminal (see the error-seam note on the type).
func (s *ProdReconcileStore) Err() error { return s.err }

// fail records the first infrastructure error (sticky) and returns it for the
// caller's convenience. A guard rejection (0 rows, live foreign lease) is NOT a
// failure — those are reported through the method's boolean, not Err().
func (s *ProdReconcileStore) fail(err error) error {
	if s.err == nil {
		s.err = err
	}
	return err
}

// initiator maps the optional §12.4 user id to a NULL-able driver value.
func (s *ProdReconcileStore) initiator() any {
	if s.initiatedBy == "" {
		return nil
	}
	return s.initiatedBy
}

// Step returns the committed durable reconcile_step for this Run's claim row, or
// "" on a captured error (the caller must check Err()). "" is deliberately NOT a
// terminal step (reconcile.IsTerminal("") is false), so a read failure never reads
// as "Run finished" — the loop requeues instead.
func (s *ProdReconcileStore) Step() reconcile.Step {
	var step string
	err := s.db.QueryRowContext(s.ctx,
		`SELECT reconcile_step FROM coord.claim WHERE work_item_id = $1::uuid`,
		s.workItemID).Scan(&step)
	if err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.Step: %w", err))
		return ""
	}
	return reconcile.Step(step)
}

// Fence returns the current monotonic fence token for this Run's claim row, or 0
// on a captured error. 0 is the unheld/default fence, so a read failure cannot
// forge a higher fence that would let a stale pass' equality guard pass.
func (s *ProdReconcileStore) Fence() int64 {
	var fence int64
	err := s.db.QueryRowContext(s.ctx,
		`SELECT fence_token FROM coord.claim WHERE work_item_id = $1::uuid`,
		s.workItemID).Scan(&fence)
	if err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.Fence: %w", err))
		return 0
	}
	return fence
}

// Advance is the §6.4 conditional step-advance: one transaction co-committing the
// guarded coord.claim UPDATE (step-CAS + fence equality), the §6.5 audit row and
// the §6.6 outbox row (AC6). It returns true IFF the guarded UPDATE committed its
// one row. A nil fence takes NO fence-equality guard (the naive model); a non-nil
// fence requires fence_token = *fence. A guard rejection returns false with no
// captured error; an infrastructure failure returns false AND captures Err().
func (s *ProdReconcileStore) Advance(expected, next reconcile.Step, fence *int64) bool {
	tx, err := s.db.BeginTx(s.ctx, nil)
	if err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.Advance: begin: %w", err))
		return false
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// (1) The guarded advance. A nil fence disables the equality guard via the
	//     ($3::bigint IS NULL OR fence_token = $3) branch — the step-CAS alone then
	//     serializes, which is exactly the naive-reconciler model. RETURNING the
	//     post-advance fence stamps the audit/outbox rows with the fence the
	//     transition committed under.
	var fenceAfter int64
	var fenceArg any
	if fence != nil {
		fenceArg = *fence
	}
	switch err := tx.QueryRowContext(s.ctx, `
		UPDATE coord.claim
		   SET reconcile_step = $2
		 WHERE work_item_id = $1::uuid
		   AND reconcile_step = $4
		   AND ($3::bigint IS NULL OR fence_token = $3::bigint)
		 RETURNING fence_token`,
		s.workItemID, string(next), fenceArg, string(expected),
	).Scan(&fenceAfter); {
	case errors.Is(err, sql.ErrNoRows):
		return false // stale/duplicate/zombie pass: expected step or fence no longer holds — commit nothing
	case err != nil:
		s.fail(fmt.Errorf("coord.ProdReconcileStore.Advance: update: %w", err))
		return false
	}

	// (2) §6.5 audit row — the coordination-audit record of the transition, in the
	//     SAME transaction so an advanced-but-unaudited step cannot exist.
	if _, err := tx.ExecContext(s.ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal,
		        initiated_by_user_id, fence_token, from_state, to_state)
		VALUES ($1::uuid, $2::uuid, 'reconcile_advanced', $3,
		        $4::uuid, $5, $6, $7)`,
		s.workItemID, s.runID, s.principal, s.initiator(), fenceAfter,
		string(expected), string(next)); err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.Advance: audit: %w", err))
		return false
	}

	// (3) §6.6 outbox row — the exactly-once projection event, co-committed so a
	//     downstream relay (CRD status patch, SSE) can never observe a step advance
	//     that has no projection event, nor an event for an advance that rolled back.
	if _, err := tx.ExecContext(s.ctx, `
		INSERT INTO coord.outbox
		       (work_item_id, run_id, event_type, from_step, to_step, fence_token)
		VALUES ($1::uuid, $2::uuid, 'reconcile_advanced', $3, $4, $5)`,
		s.workItemID, s.runID, string(expected), string(next), fenceAfter); err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.Advance: outbox: %w", err))
		return false
	}

	if err := tx.Commit(); err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.Advance: commit: %w", err))
		return false
	}
	return true
}

// Reclaim is the §6.3 fence-first reclaim: bump the fence out (fencing the dead
// holder) and stamp reclaim_fenced_at, BEFORE the failover leader re-enters. It is
// MONOTONIC — the WHERE fence_token < :new guard makes a newFence that would not
// raise the token a no-op (returns false), so two reclaimers converge and neither
// lowers the fence. Returns false with no captured error when the token was not
// raised; captures Err() on infrastructure failure.
func (s *ProdReconcileStore) Reclaim(newFence int64) bool {
	res, err := s.db.ExecContext(s.ctx, `
		UPDATE coord.claim
		   SET fence_token = $2, reclaim_fenced_at = clock_timestamp()
		 WHERE work_item_id = $1::uuid AND fence_token < $2`,
		s.workItemID, newFence)
	if err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.Reclaim: %w", err))
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.Reclaim: rows: %w", err))
		return false
	}
	return n == 1
}

// SetStep re-points the durable step WITHOUT a guard — used only for the §8
// Failed→Claiming retry re-entry AFTER a fence-first reclaim (a caller decision
// bounded by retryPolicy), never on the happy path. It is not transactional with
// any audit/outbox row: the retry lap's subsequent Advance calls carry the
// provenance. An infrastructure failure is captured in Err().
func (s *ProdReconcileStore) SetStep(step reconcile.Step) {
	if _, err := s.db.ExecContext(s.ctx,
		`UPDATE coord.claim SET reconcile_step = $2 WHERE work_item_id = $1::uuid`,
		s.workItemID, string(step)); err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.SetStep: %w", err))
	}
}

// AuditRows returns the count of reconcile-advance audit rows for this Run's item —
// the §6.5 half of the co-commit the falsification asserts is exactly one per
// committed transition (AC6). Returns 0 and captures Err() on failure.
func (s *ProdReconcileStore) AuditRows() int {
	var n int
	if err := s.db.QueryRowContext(s.ctx, `
		SELECT count(*) FROM coord.audit_log
		 WHERE work_item_id = $1::uuid AND event_type = 'reconcile_advanced'`,
		s.workItemID).Scan(&n); err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.AuditRows: %w", err))
		return 0
	}
	return n
}

// OutboxRows returns the count of reconcile-advance outbox rows for this Run's item
// — the §6.6 half of the co-commit (AC6). Returns 0 and captures Err() on failure.
func (s *ProdReconcileStore) OutboxRows() int {
	var n int
	if err := s.db.QueryRowContext(s.ctx, `
		SELECT count(*) FROM coord.outbox
		 WHERE work_item_id = $1::uuid AND event_type = 'reconcile_advanced'`,
		s.workItemID).Scan(&n); err != nil {
		s.fail(fmt.Errorf("coord.ProdReconcileStore.OutboxRows: %w", err))
		return 0
	}
	return n
}

// Compile-time proof that ProdReconcileStore satisfies the reconcile.Store seam the
// machine drives — if the interface grows a method, this fails to build here, at
// the binding, not at some distant call site.
var _ reconcile.Store = (*ProdReconcileStore)(nil)
