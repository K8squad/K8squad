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

// a2asettle.go — the durable a2a follow-settlement writer (ADR-0020 §2.2,
// ISI-4348-S1). When a dispatcher follow completes, its OnDone seam persists an
// at-most-once marker on the dispatch lap (migration 0019: settled_at +
// settle_outcome on coord.a2a_dispatch) so a post-restart reaper (S2) can tell a
// finished-and-settled run-owned sandbox pod (reap) from an agent still working
// (keep). Without the durable marker, OnDone's in-memory pool.Release (ISI-4346)
// is process-local: an operator restart mid-run kills the follow goroutine and
// the pod leaks forever.
//
// This is deliberately NOT a method on ProdEffects. ProdEffects is bound to one
// Run's coord rows at construction (workItemID/runID/principal) and is driven
// inside the §6.4 reconcile loop; settlement is a POST-terminal annotation
// written on the dispatcher's background follow goroutine, which knows only
// (a2aTaskID, runID) — orthogonal to the step machine (ADR-0020 §3). So it gets
// its own small writer, the same shape as the sibling coord writers
// (ProdCancelStore, ProdHandoffWriter, ProdDispatcher).
//
// Ordering invariant (ADR-0020 §2.2): the caller MUST write this durable marker
// FIRST, then do the best-effort in-memory pool.Release. If the process dies
// between the two, the reaper still has the marker and finishes teardown; if
// Release succeeds, the pod (and its task-io Secret) are gone and the reaper
// never sees it. Either way, no leak.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// The closed set of a2a follow-settlement outcomes (ADR-0020 §2.1). These are the
// ONLY values migration 0019's settle_outcome CHECK admits — keep them in lockstep
// with the migration and with internal/a2a.SettleOutcome (the Result → outcome map).
const (
	// SettleOutcomeSucceeded: the follow reached a clean terminal Completed state.
	SettleOutcomeSucceeded = "succeeded"
	// SettleOutcomeFailed: the follow reached a clean terminal non-success state.
	SettleOutcomeFailed = "failed"
	// SettleOutcomeFollowError: the follow ended on an SSE/transport error — the
	// stream broke, which is NOT proof the agent finished (the caller leaves the
	// pod in place), but the marker still records that the follow ended.
	SettleOutcomeFollowError = "follow_error"
)

// ProdSettler writes the durable, at-most-once follow-settlement marker for a
// dispatch lap and its §6.5 audit provenance. It is safe for concurrent use
// (each Settle runs in its own short transaction).
type ProdSettler struct {
	db        *sql.DB
	principal string // §6.5 provenance on the a2a_settled audit row
}

// NewProdSettleWriter binds the settlement writer to the coord Postgres. principal
// is the actor stamped on the a2a_settled audit row (the operator's own principal
// — settlement is control-plane-driven, so initiated_by_user_id stays NULL).
func NewProdSettleWriter(db *sql.DB, principal string) (*ProdSettler, error) {
	if db == nil {
		return nil, errors.New("coord.NewProdSettleWriter: nil db")
	}
	if principal == "" {
		return nil, errors.New("coord.NewProdSettleWriter: empty principal")
	}
	return &ProdSettler{db: db, principal: principal}, nil
}

// Settle records the durable follow-settlement marker for one dispatch lap
// (keyed by a2aTaskID) and, ON THE FIRST WRITER ONLY, an 'a2a_settled' §6.5 audit
// row — both in ONE transaction so the audit lands exactly once iff the marker
// was written.
//
// At-most-once (ADR-0020 §2.2, F2): the conditional UPDATE ... WHERE
// settled_at IS NULL is the single-statement guard — a re-entry (e.g. a
// duplicated OnDone across a restart boundary, or a lap already settled by a
// concurrent writer) matches nothing, writes no marker and no audit, and returns
// nil. RETURNING work_item_id both proves exactly one row moved unsettled →
// settled AND hands the audit the row's own item id with no second read.
//
// An unknown / never-dispatched a2aTaskID is likewise a silent no-op (no row to
// settle) — Settle never fabricates a dispatch row.
func (s *ProdSettler) Settle(ctx context.Context, a2aTaskID, runID, outcome string) error {
	switch outcome {
	case SettleOutcomeSucceeded, SettleOutcomeFailed, SettleOutcomeFollowError:
	default:
		return fmt.Errorf("coord.ProdSettler.Settle: invalid outcome %q (want succeeded|failed|follow_error)", outcome)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("coord.ProdSettler.Settle: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var workItemID string
	switch err := tx.QueryRowContext(ctx, `
		UPDATE coord.a2a_dispatch
		   SET settled_at = now(), settle_outcome = $2
		 WHERE a2a_task_id = $1 AND settled_at IS NULL
		 RETURNING work_item_id`,
		a2aTaskID, outcome).Scan(&workItemID); {
	case errors.Is(err, sql.ErrNoRows):
		// Already settled, or an unknown lap: no marker, no audit. Idempotent
		// no-op — the empty tx rolls back via the defer.
		return nil
	case err != nil:
		return fmt.Errorf("coord.ProdSettler.Settle: mark: %w", err)
	}

	// §6.5 provenance on the canonical coord.audit_log — emitted exactly once,
	// gated on the single row the conditional UPDATE moved. to_state carries the
	// outcome; run_id is the settled Run; initiated_by_user_id stays NULL
	// (control-plane-driven, no human initiator).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, to_state)
		VALUES ($1::uuid, $2::uuid, 'a2a_settled', $3, $4)`,
		workItemID, runID, s.principal, outcome); err != nil {
		return fmt.Errorf("coord.ProdSettler.Settle: audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("coord.ProdSettler.Settle: commit: %w", err)
	}
	return nil
}

// ProdSettleReader is the read side of the follow-settlement marker (ADR-0020
// §2.3, ISI-4348-S2). The restart-safe reaper asks it "has this run's a2a follow
// durably settled?" for a run-owned sandbox pod; a true answer, combined with
// the dispatcher's in-process not-following oracle, is the provably-safe signal
// to reap. It is a distinct type from ProdSettler (write vs read) but shares the
// coord Postgres.
type ProdSettleReader struct {
	db *sql.DB
}

// NewProdSettleReader binds the settlement reader to the coord Postgres.
func NewProdSettleReader(db *sql.DB) (*ProdSettleReader, error) {
	if db == nil {
		return nil, errors.New("coord.NewProdSettleReader: nil db")
	}
	return &ProdSettleReader{db: db}, nil
}

// Settled reports whether ANY dispatch lap of runID carries the durable
// follow-settlement marker (settled_at IS NOT NULL). A run may have several laps
// (a re-drive mints run_id#lapN, C1); the reaper's question is "did this run's
// follow reach completion at all", so any one settled lap answers yes. The query
// rides the partial idx_a2a_dispatch_settled index (run_id WHERE settled_at IS
// NOT NULL) — one indexed EXISTS, cheap enough for the warm-tick backstop.
//
// An unknown / never-dispatched runID returns (false, nil): no lap, not settled.
func (r *ProdSettleReader) Settled(ctx context.Context, runID string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var settled bool
	if err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM coord.a2a_dispatch
			 WHERE run_id = $1::uuid AND settled_at IS NOT NULL)`,
		runID).Scan(&settled); err != nil {
		return false, fmt.Errorf("coord.ProdSettleReader.Settled: query run %s: %w", runID, err)
	}
	return settled, nil
}

// SettledForWorkItem is the S3 read path (ADR-0020 §2.4, ISI-4403). Keyed by a
// Run's spec.workItemRef — which IS coord.a2a_dispatch.work_item_id (0005: the
// driver parses it as a uuid and hands the SAME id to the reconcile-step reader,
// ADR-001) — it answers BOTH questions the Run status projector needs to hold the
// phase honest across the a2a finalize window:
//
//   - dispatched=false → no a2a_dispatch row for this work item: the Run is not an
//     a2a follow (or was never dispatched). There is no finalize window, so the
//     projector flips to the terminal phase on the durable step alone. A "" or a
//     non-uuid workItemID (ISI-4354, a malformed ref on a pre-validation CR) is
//     likewise dispatched=false WITHOUT touching the DB — the ::uuid cast would
//     otherwise reject it (22P02) and stall the projector in error backoff.
//   - dispatched=true, settled=false → a follow is in flight (dispatched, not yet
//     observed to complete): the finalize window is open, hold the phase at Running.
//   - dispatched=true, settled=true → the follow completed durably. A run may have
//     several laps (a re-drive mints run_id#lapN, §8); the question is "did this
//     run's follow reach completion at all", so bool_or across laps answers yes.
//
// One indexed aggregate scan rides idx_a2a_dispatch_run (work_item_id leading,
// 0005) — count/bool_or over the empty set yield (0, NULL→false), so the query
// returns exactly one row and never sql.ErrNoRows.
func (r *ProdSettleReader) SettledForWorkItem(ctx context.Context, workItemID string) (dispatched, settled bool, err error) {
	if r == nil || r.db == nil {
		return false, false, errors.New("coord.ProdSettleReader: nil db")
	}
	if workItemID == "" {
		return false, false, nil
	}
	if _, perr := uuid.Parse(workItemID); perr != nil {
		return false, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := r.db.QueryRowContext(ctx, `
		SELECT count(*) > 0, COALESCE(bool_or(settled_at IS NOT NULL), false)
		  FROM coord.a2a_dispatch
		 WHERE work_item_id = $1::uuid`,
		workItemID).Scan(&dispatched, &settled); err != nil {
		return false, false, fmt.Errorf("coord.ProdSettleReader.SettledForWorkItem: query work item %s: %w", workItemID, err)
	}
	return dispatched, settled, nil
}
