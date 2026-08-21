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

// cancelprod.go — the Story 3.3 operator-kill seam over the production coord
// schema (ISI-2884). Kill is a TWO-TRANSITION protocol on the durable machine:
//
//	CancelEnter  (apiserver, on kill)    running-ish → cancelling, fence-first:
//	                                   the fence bump zombies any live agent
//	                                   (its next beat fails the fence, §6.3)
//	                                   and the work-item checkout is released
//	                                   for reclaim — the item is available the
//	                                   moment the operator says kill.
//	CancelFinish (operator drive loop)   cancelling → cancelled, after the
//	                                   sandbox teardown: terminal, audited.
//
// Both transitions are ONE transaction co-committing the claim UPDATE, its
// §6.5 audit row (with the initiating USER for Enter — a kill is a
// human-initiated action, unlike the operator-principal re-entries) and the
// §6.6 outbox event. ok=false means the expected fence no longer held (a retry
// lap raced in) or the Run went terminal — commit nothing, the caller re-reads
// and decides. A terminal Run is never resurrected (AC5).
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// cancelTerminalSet mirrors the machine's absorbing steps: the Enter/Finish
// UPDATEs refuse to move a terminal claim (AC5).
const cancelTerminalSet = `('succeeded','failed','cancelled')`

// CancelOutcome reports what the Enter transition did.
type CancelOutcome string

const (
	// CancelAccepted: the claim moved to cancelling (fence bumped, checkout
	// released). The drive loop will tear the sandbox down and finish.
	CancelAccepted CancelOutcome = "accepted"
	// CancelConflict: the expected fence no longer held (a retry lap or
	// another kill raced). Re-read and retry — never blind-force.
	CancelConflict CancelOutcome = "conflict"
	// CancelTerminal: the Run already reached a terminal step. A terminal Run
	// is never resurrected (AC5) — report the step.
	CancelTerminal CancelOutcome = "terminal"
	// CancelMissing: no claim row exists for the work item.
	CancelMissing CancelOutcome = "missing"
)

// CancelState is the claim snapshot the kill API reads before entering.
type CancelState struct {
	Step  string
	Fence int64
}

// ProdCancelStore is the production 3.3 kill seam. The apiserver binds it for
// the kill API (Enter); the operator's drive loop binds it for the teardown
// finish and the cancelling sweep (Finish/Due/State).
type ProdCancelStore struct {
	db *sql.DB
}

// NewProdCancelStore binds the kill seam over the coord schema.
func NewProdCancelStore(db *sql.DB) *ProdCancelStore { return &ProdCancelStore{db: db} }

// State reads the claim snapshot for the kill decision.
func (s *ProdCancelStore) State(ctx context.Context, workItemID string) (CancelState, bool, error) {
	var cs CancelState
	err := s.db.QueryRowContext(ctx, `
		SELECT reconcile_step, fence_token
		  FROM coord.claim WHERE work_item_id = $1::uuid`, workItemID).
		Scan(&cs.Step, &cs.Fence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return CancelState{}, false, nil
	case err != nil:
		return CancelState{}, false, fmt.Errorf("coord.ProdCancelStore.State: %w", err)
	}
	return cs, true, nil
}

// CancelEnter is the kill-side transition: fence-first move to cancelling with
// the checkout released. initiatedBy stamps the calling user on the audit row
// (§6.5: a kill is human-initiated; empty falls back to the server principal).
// runID may be empty — audit_log.run_id is nullable; the outbox resolves it
// from the latest dispatch marker when absent.
func (s *ProdCancelStore) CancelEnter(ctx context.Context, workItemID, runID, principal, initiatedBy string, fromFence int64) (CancelOutcome, error) {
	switch o, err := s.cancelTx(ctx, workItemID, runID, principal, initiatedBy,
		"cancel_requested", "cancelling", fromFence, true); {
	case err != nil:
		return CancelMissing, err
	case o == CancelAccepted:
		return CancelAccepted, nil
	default:
		return o, nil
	}
}

// CancelFinish is the drive-loop-side transition after the sandbox teardown:
// cancelling → cancelled (terminal), fence-first, audited.
func (s *ProdCancelStore) CancelFinish(ctx context.Context, workItemID, runID, principal string, fromFence int64) (CancelOutcome, error) {
	return s.cancelTx(ctx, workItemID, runID, principal, "",
		"run_cancelled", "cancelled", fromFence, false)
}

// Due lists work items sitting at cancelling — the operator's cancel sweep
// (Story 3.3 backstop wake: kills issued while a Run was healthy need a kick;
// a 2.4-style bounded sweep is the sanctioned shape).
func (s *ProdCancelStore) Due(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT work_item_id::text FROM coord.claim
		 WHERE reconcile_step = 'cancelling'`)
	if err != nil {
		return nil, fmt.Errorf("coord.ProdCancelStore.Due: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("coord.ProdCancelStore.Due: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// cancelTx is the shared one-transaction transition (claim UPDATE + audit +
// outbox), same co-commit discipline as the rundrive re-entries. releaseCheckout
// distinguishes Enter (release: the item is reclaimable immediately) from
// Finish (already released at Enter).
func (s *ProdCancelStore) cancelTx(ctx context.Context, workItemID, runID, principal, initiatedBy, event, toStep string, fromFence int64, releaseCheckout bool) (CancelOutcome, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CancelMissing, fmt.Errorf("coord.ProdCancelStore.%s: begin: %w", event, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	release := ""
	if releaseCheckout {
		release = `, holder_principal = NULL, lease_expires_at = NULL`
	}
	q := fmt.Sprintf(`
		UPDATE coord.claim
		   SET fence_token       = fence_token + 1,
		       reclaim_fenced_at = clock_timestamp()%s,
		       reconcile_step    = $3
		 WHERE work_item_id = $1::uuid
		   AND fence_token   = $2
		   AND reconcile_step NOT IN %s
		 RETURNING fence_token, reconcile_step`, release, cancelTerminalSet)
	var fenceAfter int64
	var stepAfter string
	switch err := tx.QueryRowContext(ctx, q, workItemID, fromFence, toStep).Scan(&fenceAfter, &stepAfter); {
	case errors.Is(err, sql.ErrNoRows):
		// Distinguish terminal (never resurrect) from fence conflict (retry).
		var cur string
		if err := tx.QueryRowContext(ctx,
			`SELECT reconcile_step FROM coord.claim WHERE work_item_id = $1::uuid`, workItemID).
			Scan(&cur); err == nil && isCancelTerminalStep(cur) {
			return CancelTerminal, nil
		}
		return CancelConflict, nil
	case err != nil:
		return CancelMissing, fmt.Errorf("coord.ProdCancelStore.%s: update: %w", event, err)
	}

	if runID == "" {
		runID = cancelRunIDFor(ctx, tx, workItemID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal,
		        initiated_by_user_id, fence_token, to_state)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, NULLIF($5,''), $6, $7)`,
		workItemID, runID, event, principal, initiatedBy, fenceAfter, toStep); err != nil {
		return CancelMissing, fmt.Errorf("coord.ProdCancelStore.%s: audit: %w", event, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.outbox
		       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
		SELECT 'run', wi.project_id, wi.team_id::text, $3,
		       wi.id, NULLIF($2,'')::uuid,
		       jsonb_build_object('to_step', $4::text, 'fence_token', $5::bigint)
		  FROM coord.work_item wi WHERE wi.id = $1::uuid`,
		workItemID, runID, event, toStep, fenceAfter); err != nil {
		return CancelMissing, fmt.Errorf("coord.ProdCancelStore.%s: outbox: %w", event, err)
	}

	if err := tx.Commit(); err != nil {
		return CancelMissing, fmt.Errorf("coord.ProdCancelStore.%s: commit: %w", event, err)
	}
	return CancelAccepted, nil
}

// cancelRunIDFor resolves the run that was executing when the kill landed (the
// latest dispatch marker) — audit provenance when the caller carries none.
func cancelRunIDFor(ctx context.Context, q interface {
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

func isCancelTerminalStep(step string) bool {
	return step == "succeeded" || step == "failed" || step == "cancelled"
}
