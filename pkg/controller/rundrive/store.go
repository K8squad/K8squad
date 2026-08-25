/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// store.go — the production bindings of the rundrive seams (ISI-2883): the
// Claims surface (claim-row reads, guarded retry/fail/resume re-entries over
// the checked-in coord schema), the Pauses surface (a thin adapter over
// coord.ProdResumeStore), the Runner factory (per-Run ProdReconcileStore +
// ProdEffects), and the spec-driven warm-pool RunClassifier.
package rundrive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/warmpool"
)

// OperatorPrincipal is the control-plane principal the operator drives as
// (stamped on every audit/outbox row the driver commits, §6.5).
const OperatorPrincipal = "ksquad-operator"

// terminalSet is the absorbing-step guard every re-entry's UPDATE carries: a
// terminal Run is never resurrected (AC5).
const terminalSet = `('succeeded','failed','cancelled')`

// ProdClaims implements Claims over the production coord schema.
type ProdClaims struct {
	db        *sql.DB
	principal string
}

// NewProdClaims binds the Claims seam. principal defaults to OperatorPrincipal.
func NewProdClaims(db *sql.DB, principal string) *ProdClaims {
	if principal == "" {
		principal = OperatorPrincipal
	}
	return &ProdClaims{db: db, principal: principal}
}

// State reads the claim-row snapshot one drive pass decides on.
func (c *ProdClaims) State(ctx context.Context, workItemID string) (ClaimState, bool, error) {
	var cs ClaimState
	var holder sql.NullString
	var lease sql.NullTime
	err := c.db.QueryRowContext(ctx, `
		SELECT reconcile_step, fence_token, holder_principal, lease_expires_at
		  FROM coord.claim WHERE work_item_id = $1::uuid`, workItemID).
		Scan(&cs.Step, &cs.Fence, &holder, &lease)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ClaimState{}, false, nil
	case err != nil:
		return ClaimState{}, false, fmt.Errorf("rundrive.ProdClaims.State: %w", err)
	}
	cs.Holder = holder.String
	if lease.Valid {
		t := lease.Time
		cs.LeaseExpiresAt = &t
	}
	return cs, true, nil
}

// LapsUsed counts completed retry-lap dispatch markers (run_id#lapN rows in
// coord.a2a_dispatch) — the durable retry budget ledger: no separate counter,
// the at-most-once dispatch markers ARE the lap history.
func (c *ProdClaims) LapsUsed(ctx context.Context, runID string) (int, error) {
	var laps int
	if err := c.db.QueryRowContext(ctx, `
		SELECT count(*) FROM coord.a2a_dispatch
		 WHERE run_id = $1::uuid AND a2a_task_id LIKE '%#lap%'`, runID).
		Scan(&laps); err != nil {
		return 0, fmt.Errorf("rundrive.ProdClaims.LapsUsed: %w", err)
	}
	return laps, nil
}

// enter is the shared §5.3 re-entry: ONE transaction co-committing the
// fence-first claim UPDATE (bump the fence — fencing any zombie §6.3 —
// release the work-item checkout, move the step), its §6.5 audit row and
// §6.6 outbox event. Same co-commit discipline as ProdReconcileStore.Advance:
// a re-entry is never half-committed. ok=false means the expected fence no
// longer held (someone else reclaimed) or the Run went terminal — commit
// nothing. stepClause is empty (move only via set below) or a SQL fragment
// `, reconcile_step = '...'`.
func (c *ProdClaims) enter(ctx context.Context, workItemID, runID, event string, fromFence int64, stepClause, toState string) (int64, bool, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("rundrive.ProdClaims.%s: begin: %w", event, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var fenceAfter int64
	q := fmt.Sprintf(`
		UPDATE coord.claim
		   SET fence_token       = fence_token + 1,
		       reclaim_fenced_at = clock_timestamp(),
		       holder_principal  = NULL,
		       lease_expires_at  = NULL%s
		 WHERE work_item_id = $1::uuid
		   AND fence_token   = $2
		   AND reconcile_step NOT IN %s
		 RETURNING fence_token`, stepClause, terminalSet)
	switch err := tx.QueryRowContext(ctx, q, workItemID, fromFence).Scan(&fenceAfter); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("rundrive.ProdClaims.%s: update: %w", event, err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal,
		        initiated_by_user_id, fence_token, to_state)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, NULL, $5, $6)`,
		workItemID, runID, event, c.principal, fenceAfter, toState); err != nil {
		return 0, false, fmt.Errorf("rundrive.ProdClaims.%s: audit: %w", event, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.outbox
		       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
		SELECT 'run', wi.project_id, wi.team_id::text, $3,
		       wi.id, NULLIF($2,'')::uuid,
		       jsonb_build_object('to_step', $4::text, 'fence_token', $5::bigint)
		  FROM coord.work_item wi WHERE wi.id = $1::uuid`,
		workItemID, runID, event, toState, fenceAfter); err != nil {
		return 0, false, fmt.Errorf("rundrive.ProdClaims.%s: outbox: %w", event, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("rundrive.ProdClaims.%s: commit: %w", event, err)
	}
	return fenceAfter, true, nil
}

// RetryEnter implements Claims.RetryEnter: §6.3 fence-first reclaim + checkout
// release + step → claiming_sandbox (the §8 retry lap re-entry), co-committing
// the audit + outbox rows.
func (c *ProdClaims) RetryEnter(ctx context.Context, workItemID, runID string, fromFence int64) (int64, bool, error) {
	return c.enter(ctx, workItemID, runID, "retry_lap_entered", fromFence,
		`, reconcile_step = 'claiming_sandbox'`, "claiming_sandbox")
}

// FailEnter implements Claims.FailEnter: same custody fix, step → failed.
func (c *ProdClaims) FailEnter(ctx context.Context, workItemID, runID string, fromFence int64) (bool, error) {
	_, ok, err := c.enter(ctx, workItemID, runID, "run_failed_entered", fromFence,
		`, reconcile_step = 'failed'`, "failed")
	return ok, err
}

// CancelEnter implements Claims.CancelEnter (ISI-2884): same custody fix as the
// fail path, step → cancelled. Terminal, so the checkout is released.
func (c *ProdClaims) CancelEnter(ctx context.Context, workItemID, runID string, fromFence int64) (bool, error) {
	_, ok, err := c.enter(ctx, workItemID, runID, "run_cancelled_entered", fromFence,
		`, reconcile_step = 'cancelled'`, "cancelled")
	return ok, err
}

// runIDFor looks up the Run's uuid for the audit/outbox provenance: the
// latest dispatch marker for the item (the run that was executing when the
// episode landed). Empty when none exists — audit_log.run_id is nullable.
func (c *ProdClaims) runIDFor(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, workItemID string) string {
	var runID sql.NullString
	if err := q.QueryRowContext(ctx, `
		SELECT run_id::text FROM coord.a2a_dispatch
		 WHERE work_item_id = $1::uuid
		 ORDER BY a2a_task_id DESC LIMIT 1`, workItemID).Scan(&runID); err != nil {
		return ""
	}
	return runID.String
}

// RequeuePaused implements Claims.RequeuePaused: the 3.7 resume re-entry —
// guarded paused(rate_limited) → dispatching, custody RETAINED (the §8
// short-pause rule: the checkout stays held; only the step moves), audited.
func (c *ProdClaims) RequeuePaused(ctx context.Context, workItemID string) (bool, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("rundrive.ProdClaims.RequeuePaused: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE coord.claim
		   SET reconcile_step = 'dispatching'
		 WHERE work_item_id = $1::uuid
		   AND reconcile_step = 'paused(rate_limited)'`, workItemID)
	if err != nil {
		return false, fmt.Errorf("rundrive.ProdClaims.RequeuePaused: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rundrive.ProdClaims.RequeuePaused: rows: %w", err)
	}
	if n == 0 {
		return false, nil // already moved on (or never parked)
	}

	runID := c.runIDFor(ctx, tx, workItemID)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, from_state, to_state)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, 'resume_requeued', $3,
		        'paused(rate_limited)', 'dispatching')`,
		workItemID, runID, c.principal); err != nil {
		return false, fmt.Errorf("rundrive.ProdClaims.RequeuePaused: audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.outbox
		       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
		SELECT 'run', wi.project_id, wi.team_id::text, 'resume_requeued',
		       wi.id, NULLIF($2,'')::uuid,
		       jsonb_build_object('from_step', 'paused(rate_limited)',
		                          'to_step', 'dispatching')
		  FROM coord.work_item wi WHERE wi.id = $1::uuid`,
		workItemID, runID); err != nil {
		return false, fmt.Errorf("rundrive.ProdClaims.RequeuePaused: outbox: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("rundrive.ProdClaims.RequeuePaused: commit: %w", err)
	}
	return true, nil
}

// ProdPauses adapts coord.ProdResumeStore to the Pauses seam.
type ProdPauses struct{ store *coord.ProdResumeStore }

// NewProdPauses binds the Pauses seam over the production resume store.
func NewProdPauses(store *coord.ProdResumeStore) *ProdPauses { return &ProdPauses{store: store} }

// Pending implements Pauses.Pending.
func (p *ProdPauses) Pending(ctx context.Context, workItemID string) (time.Time, bool, error) {
	return p.store.Pending(ctx, workItemID)
}

// Record implements Pauses.Record.
func (p *ProdPauses) Record(ctx context.Context, workItemID, runID string, retryAfter *time.Duration) (coord.PauseInfo, error) {
	return p.store.Pause(ctx, workItemID, runID, retryAfter)
}

// ProdRunner constructs the per-Run machine bindings (Runner seam): the
// ProdReconcileStore/ProdEffects pair keyed on the Run's REAL identifiers
// (workItemRef + uid), with the optional physical SandboxBinder /
// TaskDispatcher ports (nil = ledger-only, the honest pre-shim mode).
type ProdRunner struct {
	db          *sql.DB
	principal   string
	initiatedBy string
	binder      coord.SandboxBinder
	dispatcher  coord.TaskDispatcher
}

// NewProdRunner binds the Runner seam. binder/dispatcher may be nil.
func NewProdRunner(db *sql.DB, principal string, binder coord.SandboxBinder, dispatcher coord.TaskDispatcher) *ProdRunner {
	if principal == "" {
		principal = OperatorPrincipal
	}
	return &ProdRunner{db: db, principal: principal, binder: binder, dispatcher: dispatcher}
}

// Store implements Runner.Store.
func (r *ProdRunner) Store(ctx context.Context, run *api.Run) (machineStore, error) {
	return coord.NewProdReconcileStore(ctx, r.db, run.Spec.WorkItemRef, string(run.UID),
		r.principal, r.initiatedBy)
}

// Effects implements Runner.Effects.
func (r *ProdRunner) Effects(ctx context.Context, run *api.Run) (machineEffects, error) {
	return coord.NewProdEffects(ctx, r.db, run.Spec.WorkItemRef, string(run.UID),
		r.principal, r.initiatedBy, r.binder, r.dispatcher)
}

// SpecClassifier resolves a Run's warm-pool (key, class) from its CRD spec —
// a warmpool.RunClassifier the operator wiring hands to warmpool.NewBinder.
// RuntimeClass/Class come from spec.sandboxPolicy with the story 1.3 admission
// defaults (gvisor/interactive) applied read-side, so the classifier is
// correct even for Runs admitted before defaulting landed. The image dimension
// stays "" until the Agent-runtime image resolution lands (ISI-2889) — the
// single-key pool regime warmpool.DefaultClassifier also pins.
func SpecClassifier(reader client.Reader) warmpool.RunClassifier {
	return func(ctx context.Context, runID string) (warmpool.PoolKey, warmpool.RunClass, error) {
		key := warmpool.PoolKey{RuntimeClass: "gvisor"}
		class := warmpool.ClassInteractive
		// The binder hands the driver's runID (the Run CRD uid); resolve the
		// spec read-side. A Run deleted mid-bind classifies on defaults.
		var runs api.RunList
		if err := reader.List(ctx, &runs); err != nil {
			return key, class, nil // defaults: classify never blocks a bind
		}
		for i := range runs.Items {
			if string(runs.Items[i].UID) != runID {
				continue
			}
			if rc := runs.Items[i].Spec.SandboxPolicy.RuntimeClass; rc != "" {
				key.RuntimeClass = rc
			}
			if runs.Items[i].Spec.SandboxPolicy.Class == "batch" {
				class = warmpool.ClassBatch
			}
			return key, class, nil
		}
		return key, class, nil
	}
}
