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

	"github.com/google/uuid"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/warmpool"
	"github.com/K8squad/K8squad/pkg/workspace"
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
	// claimer is the §6.2 production claim writer (prodclaim.go) the M1.3
	// drive loop acquires through (ISI-4183): the guarded in-place checkout
	// rewrite + todo → in_progress lane advance + claim_acquired audit +
	// claimed outbox event, ONE transaction. WithOutboxCapture is ON here —
	// the operator's database carries the full migration set (0003 outbox
	// included), unlike the 0001-only base spine chaos gate.
	claimer  *coord.ProdClaimer
	claimErr error // construction failure, surfaced by Acquire (rare: default config cannot fail)
}

// NewProdClaims binds the Claims seam. principal defaults to OperatorPrincipal.
func NewProdClaims(db *sql.DB, principal string) *ProdClaims {
	if principal == "" {
		principal = OperatorPrincipal
	}
	c := &ProdClaims{db: db, principal: principal}
	claimer, err := coord.NewProdClaimer(db, coord.DefaultProdConfig(), coord.WithOutboxCapture())
	if err != nil {
		c.claimErr = fmt.Errorf("rundrive.NewProdClaims: %w", err)
		return c
	}
	c.claimer = claimer
	return c
}

// State reads the claim-row snapshot one drive pass decides on — step, fence,
// holder, lease, the holder RUN (coord.claim.run_id, the acquire stamp M1.3
// owns) and the work item's board lane (coord.work_item.state).
//
// ISI-4354: a workItemID that does not parse as a uuid (the empty string
// included, mirroring ReconcileStepReader's guard) is reported as found=false
// WITHOUT touching the DB — the ::uuid cast would reject it with 22P02, a
// permanent input defect that must never become an infra error loop.
func (c *ProdClaims) State(ctx context.Context, workItemID string) (ClaimState, bool, error) {
	if _, err := uuid.Parse(workItemID); err != nil {
		return ClaimState{}, false, nil
	}
	var cs ClaimState
	var holder sql.NullString
	var lease sql.NullTime
	var runID sql.NullString
	err := c.db.QueryRowContext(ctx, `
		SELECT cl.reconcile_step, cl.fence_token, cl.holder_principal,
		       cl.lease_expires_at, cl.run_id::text, wi.state
		  FROM coord.claim cl
		  JOIN coord.work_item wi ON wi.id = cl.work_item_id
		 WHERE cl.work_item_id = $1::uuid`, workItemID).
		Scan(&cs.Step, &cs.Fence, &holder, &lease, &runID, &cs.ItemState)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ClaimState{}, false, nil
	case err != nil:
		return ClaimState{}, false, fmt.Errorf("rundrive.ProdClaims.State: %w", err)
	}
	cs.Holder = holder.String
	cs.RunID = runID.String
	if lease.Valid {
		t := lease.Time
		cs.LeaseExpiresAt = &t
	}
	return cs, true, nil
}

// Acquire implements Claims.Acquire: the §6.2 guarded acquire of THIS Run's
// work item (ProdClaimer.AcquireSpecific under the operator principal) —
// checkout rewrite (holder, run, fence bump, lease) + todo → in_progress lane
// advance + claim_acquired audit + claimed outbox event, one transaction.
// ok=false: the free-or-expired guard rejected us or the lane does not advance
// — nothing changed.
func (c *ProdClaims) Acquire(ctx context.Context, workItemID, runID string) (int64, bool, error) {
	if c.claimErr != nil {
		return 0, false, c.claimErr
	}
	_, fence, ok, err := c.claimer.AcquireSpecific(ctx, c.principal, runID, workItemID, "")
	if err != nil {
		return 0, false, fmt.Errorf("rundrive.ProdClaims.Acquire: %w", err)
	}
	return fence, ok, nil
}

// Renew implements Claims.Renew: the §6.2 lease heartbeat as ONE guarded
// UPDATE — holder + run + fence + live-lease must all match, renewed_at is
// stamped, and a terminal-lane work item (done) is never resurrected into a
// live lease. Mirrors ProdClaimer.Renew's guard family but returns the
// infrastructure error instead of panicking: the drive loop requeues on it.
func (c *ProdClaims) Renew(ctx context.Context, workItemID, runID string, fence int64) (bool, error) {
	res, err := c.db.ExecContext(ctx, `
		UPDATE coord.claim
		   SET lease_expires_at = clock_timestamp() + $5::interval,
		       renewed_at       = clock_timestamp()
		 WHERE work_item_id     = $1::uuid
		   AND holder_principal = $2
		   AND run_id           = $3::uuid
		   AND fence_token      = $4
		   AND lease_expires_at > clock_timestamp()
		   AND NOT EXISTS (
		         SELECT 1 FROM coord.work_item
		          WHERE id = $1::uuid AND state = 'done'
		       )`,
		workItemID, c.principal, runID, fence, coord.DefaultProdConfig().LeaseInterval)
	if err != nil {
		return false, fmt.Errorf("rundrive.ProdClaims.Renew: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rundrive.ProdClaims.Renew: rows: %w", err)
	}
	return n == 1, nil
}

// LapsUsed counts completed retry-lap dispatch markers (run_id#lapN rows in
// coord.a2a_dispatch) — the durable retry budget ledger: no separate counter,
// the at-most-once dispatch markers ARE the lap history.
// Enhanced to count only proper retry lap markers and avoid false positives.
func (c *ProdClaims) LapsUsed(ctx context.Context, runID string) (int, error) {
	var laps int
	if err := c.db.QueryRowContext(ctx, `
		SELECT count(*) FROM coord.a2a_dispatch
		 WHERE run_id = $1::uuid AND POSITION('#lap' IN a2a_task_id) > 0`, runID).
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

	// Pre-read under the row lock: was this checkout held before the release
	// below clears it? Only a HELD release owes a claim_released §6.5 row
	// (ISI-4183) — a re-entry over an already-unheld claim row (e.g. a crash
	// before the first acquire) releases nothing and must not fake custody
	// provenance. Same txn, FOR UPDATE: the read and the release are one fact.
	var wasHeld sql.NullString
	switch err := tx.QueryRowContext(ctx, `
		SELECT holder_principal FROM coord.claim
		 WHERE work_item_id = $1::uuid AND fence_token = $2
		 FOR UPDATE`, workItemID, fromFence).Scan(&wasHeld); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("rundrive.ProdClaims.%s: pre-read: %w", event, err)
	}

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
	// §6.5 claim_released provenance for the release this re-entry performed
	// (ISI-4183): co-committed, so a released checkout can never exist without
	// its audit row. Only written when a held checkout was actually cleared.
	if wasHeld.Valid {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO coord.audit_log
			       (work_item_id, run_id, event_type, principal,
			        initiated_by_user_id, fence_token, to_state)
			VALUES ($1::uuid, NULLIF($2,'')::uuid, 'claim_released', $3, NULL, $4, $5)`,
			workItemID, runID, c.principal, fenceAfter, toState); err != nil {
			return 0, false, fmt.Errorf("rundrive.ProdClaims.%s: claim_released audit: %w", event, err)
		}
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

// ClearSandboxBind implements SandboxBindClearer (ISI-4310): it removes the
// Run's durable coord.sandbox_bind marker — ONE transaction with a
// `sandbox_bind_cleared` §6.5 audit row carrying the gone pod's ref as
// provenance. Without this, a retry lap's BindSandbox would see the marker
// and reattach to the dead sandbox_ref forever (the marker's reattach is the
// at-most-once guard for a LIVE bind, and the precise reason a gone pod needs
// an explicit clear). Idempotent: a Run with no marker deletes nothing and
// writes no audit row, so the driver's crash-mid-recovery re-drive is a
// no-op. The delete is NOT fence-guarded — the marker is keyed by run_id
// alone (a new bind for the same run re-inserts under the same key), and the
// caller only reaches here after proving the referenced pod is gone.
func (c *ProdClaims) ClearSandboxBind(ctx context.Context, runID string) error {
	if runID == "" {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rundrive.ProdClaims.ClearSandboxBind: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var workItemID, sandboxRef sql.NullString
	switch err := tx.QueryRowContext(ctx, `
		DELETE FROM coord.sandbox_bind
		 WHERE run_id = $1::uuid
		 RETURNING work_item_id::text, sandbox_ref`, runID).Scan(&workItemID, &sandboxRef); {
	case errors.Is(err, sql.ErrNoRows):
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("rundrive.ProdClaims.ClearSandboxBind: commit (no-op): %w", err)
		}
		return nil // already cleared (or never bound) — idempotent no-op
	case err != nil:
		return fmt.Errorf("rundrive.ProdClaims.ClearSandboxBind: delete: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, to_state, payload)
		VALUES ($1::uuid, $2::uuid, 'sandbox_bind_cleared', $3, 'claiming_sandbox',
		        jsonb_build_object('sandbox_ref', $4, 'reason', 'sandbox pod not found'))`,
		workItemID, runID, c.principal, sandboxRef); err != nil {
		return fmt.Errorf("rundrive.ProdClaims.ClearSandboxBind: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rundrive.ProdClaims.ClearSandboxBind: commit: %w", err)
	}
	return nil
}

// CancelEnter implements Claims.CancelEnter (ISI-2884): same custody fix as the
// fail path, step → cancelled. Terminal, so the checkout is released.
func (c *ProdClaims) CancelEnter(ctx context.Context, workItemID, runID string, fromFence int64) (bool, error) {
	_, ok, err := c.enter(ctx, workItemID, runID, "run_cancelled_entered", fromFence,
		`, reconcile_step = 'cancelled'`, "cancelled")
	return ok, err
}

// CancelFinish implements Claims.CancelFinish (3.3): the guarded cancelling →
// cancelled transition after the sandbox teardown, over the shared coord kill
// seam (same co-commit discipline as enter).
func (c *ProdClaims) CancelFinish(ctx context.Context, workItemID, runID string, fromFence int64) (bool, error) {
	outcome, err := coord.NewProdCancelStore(c.db).CancelFinish(ctx, workItemID, runID, c.principal, fromFence)
	if err != nil {
		return false, err
	}
	return outcome == "accepted", nil
}

// CancelDue implements Claims.CancelDue: the kill sweep's backlog (work items
// at cancelling).
func (c *ProdClaims) CancelDue(ctx context.Context) ([]string, error) {
	return coord.NewProdCancelStore(c.db).Due(ctx)
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
	credWriter  coord.RunCredentialWriter
	refObserver coord.SandboxRefObserver
}

// NewProdRunner binds the Runner seam. binder/dispatcher may be nil.
func NewProdRunner(db *sql.DB, principal string, binder coord.SandboxBinder, dispatcher coord.TaskDispatcher) *ProdRunner {
	if principal == "" {
		principal = OperatorPrincipal
	}
	return &ProdRunner{db: db, principal: principal, binder: binder, dispatcher: dispatcher}
}

// WithCredentialWriter opts this runner into topology-2 (ADR-0007) Bind-path
// task-io Secret delivery: every per-Run ProdEffects it builds carries the writer,
// so BindSandbox writes the run-scoped credential right after the sandbox binds.
// Nil leaves credential-off mode. Returns r for chaining.
func (r *ProdRunner) WithCredentialWriter(w coord.RunCredentialWriter) *ProdRunner {
	r.credWriter = w
	return r
}

// WithSandboxRefObserver opts this runner into the M1.2 sandbox-ref surface:
// every per-Run ProdEffects it builds notifies the observer at Bind, so
// Run.status.sandboxRef lands the moment the sandbox binds. Nil (the zero
// value) leaves ref-silent mode. Returns r for chaining.
func (r *ProdRunner) WithSandboxRefObserver(o coord.SandboxRefObserver) *ProdRunner {
	r.refObserver = o
	return r
}

// Store implements Runner.Store.
func (r *ProdRunner) Store(ctx context.Context, run *api.Run) (machineStore, error) {
	return coord.NewProdReconcileStore(ctx, r.db, run.Spec.WorkItemRef, string(run.UID),
		r.principal, r.initiatedBy)
}

// Effects implements Runner.Effects.
func (r *ProdRunner) Effects(ctx context.Context, run *api.Run) (machineEffects, error) {
	e, err := coord.NewProdEffects(ctx, r.db, run.Spec.WorkItemRef, string(run.UID),
		r.principal, r.initiatedBy, r.binder, r.dispatcher)
	if err != nil {
		return nil, err
	}
	// Topology-2 opt-in: nil credWriter leaves credential-off mode;
	// nil refObserver leaves ref-silent mode (M1.2).
	return e.WithRunCredentialWriter(r.credWriter).WithSandboxRefObserver(r.refObserver), nil
}

// SpecClassifier resolves a Run's warm-pool (key, class) from its CRD spec —
// a warmpool.RunClassifier the operator wiring hands to warmpool.NewBinder.
// RuntimeClass/Class come from spec.sandboxPolicy with the story 1.3 admission
// defaults (gvisor/interactive) applied read-side, so the classifier is
// correct even for Runs admitted before defaulting landed. defaultRuntimeClass
// is that read-side default verbatim (the operator resolves
// KSQUAD_SANDBOX_RUNTIME_CLASS itself and passes "gvisor" when unconfigured —
// clusters without a gvisor RuntimeClass pin "runc" for the cluster default);
// Boot treats "" and "runc" as the cluster-default runtime. The image dimension resolves Run → Agent →
// AgentRuntime type → RuntimeImages (M1.2, the ISI-2889 image gap): a Run
// whose graph cannot yield an image fails the classify — and therefore the
// bind — loudly, instead of booting a pod with an empty image. The namespace
// and capability-hash dimensions are the Epic C tenancy/pooling fix
// (ADR-044 steps 7 and 9): warm pods boot in the Run's team namespace and
// identical capability envelopes share pool stock.
//
// ISI-4289: the capability hash is NORMALIZED for the bare posture — a
// stamped manifest that grants nothing (the assembler stamps pre-dispatch,
// so every no-capability Run carries the empty envelope's sha256) maps to
// the empty hash, keeping bare Runs on the bare warm stock the operator
// wires. Classification also FAILS CLOSED on an unresolvable Run (list
// error, or the Run is gone): the old never-fail defaults carried
// Namespace:"" and the provisioner booted orphan pods into the `default`
// namespace — a tenancy violation per ADR-044. A failed classify fails the
// bind loudly instead; the coord marker is not yet written, so a re-drive
// retries cleanly.
func SpecClassifier(reader client.Reader, imgs RuntimeImages, defaultRuntimeClass string) warmpool.RunClassifier {
	return func(ctx context.Context, runID string) (warmpool.PoolKey, warmpool.RunClass, error) {
		key := warmpool.PoolKey{RuntimeClass: defaultRuntimeClass}
		class := warmpool.ClassInteractive
		// The binder hands the driver's runID (the Run CRD uid); resolve the
		// spec read-side. A Run that cannot be resolved fails the classify
		// (fail closed — never boot a sandbox into `default` for a Run whose
		// namespace is unknown; ISI-4289).
		var runs api.RunList
		if err := reader.List(ctx, &runs); err != nil {
			return key, class, fmt.Errorf("rundrive.SpecClassifier: list runs: %w", err)
		}
		for i := range runs.Items {
			if string(runs.Items[i].UID) != runID {
				continue
			}
			key.Namespace = runs.Items[i].Namespace
			if rc := runs.Items[i].Spec.SandboxPolicy.RuntimeClass; rc != "" {
				key.RuntimeClass = rc
			}
			if runs.Items[i].Spec.SandboxPolicy.Class == "batch" {
				class = warmpool.ClassBatch
			}
			if m := runs.Items[i].Status.CapabilityManifest; m != nil && !capability.IsBareEnvelope(m) {
				key.CapabilityHash = m.CapabilityHash
			}
			key.ProjectPVC = projectWorkspacePVC(ctx, reader, &runs.Items[i])
			var err error
			if key, err = classifySandbox(ctx, reader, &runs.Items[i], imgs, key); err != nil {
				return key, class, fmt.Errorf("rundrive.SpecClassifier: %w", err)
			}
			return key, class, nil
		}
		return key, class, fmt.Errorf("rundrive.SpecClassifier: run %s not found (deleted mid-bind?) — refusing to boot a sandbox with unknown tenancy", runID)
	}
}

// projectWorkspacePVC resolves the Run's per-Project workspace claim name
// (ISI-4127): when the Run's Project carries spec.workspacePVC, the pool
// key gains the PVC dimension so Boot mounts the claim. A Project that is
// unreadable (mid-delete, cache lag) classifies WITHOUT the mount rather
// than blocking the bind — the classify-never-blocks contract — because a
// missing mount is recoverable by re-drive while a wedged bind is not.
func projectWorkspacePVC(ctx context.Context, reader client.Reader, run *api.Run) string {
	ref := run.Spec.ProjectRef
	if ref.Name == "" {
		return ""
	}
	ns := ref.Namespace
	if ns == "" {
		ns = run.Namespace
	}
	var project api.Project
	if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &project); err != nil {
		return ""
	}
	if project.Spec.WorkspacePVC == nil {
		return ""
	}
	return workspace.ProjectPVCName(project.Name)
}
