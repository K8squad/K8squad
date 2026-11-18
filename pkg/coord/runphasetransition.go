// runphasetransition.go — WS-1 of ISI-5744 (ISI-5746; ADR-0032): the APPEND-ONLY
// write seam for the Run phase-transition journal (coord.run_phase_transition,
// db/migrations/0037). It is the sanctioned — and only — write path for that
// table: a plain INSERT, no update/delete path, mirroring AppendChangeRef
// (changeref.go) and the audit_log discipline (§6.5). The storage enforces
// append-only structurally via the shared coord.reject_mutation trigger.
//
// WHY A NEW STORE (ADR-0032): a Run's phase lives only on the Run CR in etcd
// (the live value, overwritten each transition); coord.outbox carries only
// `thinking` progress (ProgressMirror ISI-5193). So the "runs by status over
// time" series (ISI-5738 §3) has no in-cluster history to reconstruct from —
// unlike the tickets series, which replays coord.audit_log state_transition
// rows (overviewread.go). This journal is that history: one immutable row per
// Run phase edge, replayed into daily buckets by the WS-3 reconstruction.
//
// SCOPE (WS-1): this file ships only the write seam + input guards + the phase
// enum. Operator emission on each Run transition (WS-2 / ISI-5749), the
// reconstruction read + prod wiring (WS-3 / ISI-5750), and the FE chart (WS-4 /
// ISI-5752) are out of scope and tracked separately.
//
// BACKWARD-EDGE GUARD (ISI-7541 — the journal follow-up to the ISI-7538
// crash-path flap, deliberately left out of PR #1047): RecordRunPhaseTransition
// reads the run's LAST journaled edge and the claim's durable fence before
// inserting, and suppresses a backward or same-band edge the fence cannot
// certify. A blind monotonic guard would corrupt genuine retry history
// (running→claiming IS the legitimate post-death retry-lap regression); the
// fence bump is what separates the two — see ErrRunPhaseEdgeSuppressed.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Run phase tokens — the four run-status CHIP TOKENS the FE renders, lowercased
// (match internal/apiserver RunsSeriesTile / RunsByStatusTile and the chip
// bucketing). The journal stores the NORMALIZED display token, not the raw Run
// CR phase (Claiming/Running/Succeeded/Failed/…): the WS-2 emitter maps CR phase
// → chip token so WS-3's reconstruction keys line up with StatusSnapshot.Counts
// with no re-mapping. Widening the set is a forward migration, never silent.
const (
	RunPhaseClaiming = "claiming"
	RunPhaseRunning  = "running"
	RunPhaseComplete = "complete"
	RunPhaseFailed   = "failed"
)

// ValidRunPhases is the closed set of coord.run_phase_transition.to_phase (and
// non-null from_phase) values — the four chip tokens above.
var ValidRunPhases = map[string]bool{
	RunPhaseClaiming: true,
	RunPhaseRunning:  true,
	RunPhaseComplete: true,
	RunPhaseFailed:   true,
}

// ErrInvalidRunPhaseTransition — the transition input is malformed (missing
// run_uid/project_id/to_phase, or a phase token outside ValidRunPhases). Maps to
// 400 on any surface that drives the seam directly; the operator emitter (WS-2)
// treats it as a programming error (it owns the CR→token mapping).
var ErrInvalidRunPhaseTransition = errors.New("coord: invalid run phase transition")

// ErrRunPhaseEdgeSuppressed — the edge was REJECTED by the backward-edge guard
// (ISI-7541): it moves the run BACKWARD (or re-enters the band the journal
// already holds) and the durable claim fence was not bumped since the last
// journaled edge — the ISI-7538 crash-path flap family (an operator restart
// re-driving a still-live claim regresses the projected phase under the SAME
// fence; RedriveClaim's no-op fast path). A bumped fence certifies a genuine
// retry lap (§6.2 acquire, §6.4 re-acquire and cancel-release all bump it), and
// only then does a backward edge journal. NOT an infrastructure failure:
// adapters may log-and-swallow it (the operator adapter does).
var ErrRunPhaseEdgeSuppressed = errors.New("coord: run phase edge suppressed by backward-edge guard")

// runPhaseRank orders the chip tokens along the state machine's one-way flow
// (claiming → running → terminal): 1 < 2 < 3. A token at a LOWER rank than the
// last journaled row's is a backward edge; complete and failed SHARE the
// terminal rank — a post-terminal token change is a settlement revision, not a
// phase regression, and journals normally.
func runPhaseRank(tok string) int {
	switch tok {
	case RunPhaseClaiming:
		return 1
	case RunPhaseRunning:
		return 2
	default:
		return 3
	}
}

// RunPhaseTransition is one observed Run phase edge — the input to the journal
// write. It is a pure value; the caller (WS-2 operator emitter) has already
// observed the transition on the Run CR.
type RunPhaseTransition struct {
	// RunUID is the Run CR metadata.uid — the per-run replay key. Required.
	RunUID string
	// TeamID / ProjectID are the Run's OWNING Team / Project CR UIDs, derived
	// from the Run CR (NOT from any linked work_item — not every Run has one).
	// ProjectID is required (tenancy predicate, §12.1); TeamID may be empty when
	// the Team is unresolved (stored NULL — a scoped read won't match it, the
	// trusted fleet-admin path still sees it).
	TeamID    string
	ProjectID string
	// WorkItemID is the Run's spec.workItemRef — the opaque coordination-DB
	// pointer (ADR-001) the backward-edge guard resolves the durable claim fence
	// through (coord.claim.fence_token, ISI-7541). NOT tenancy. Empty or
	// non-uuid ⇒ the oracle is unknown: backward edges are suppressed
	// fail-closed, forward edges always journal.
	WorkItemID string
	// FromPhase is the token the Run is LEAVING; empty ⇒ the Run's first observed
	// transition (stored NULL, like audit_log.from_state). When non-empty it must
	// be one of ValidRunPhases.
	FromPhase string
	// ToPhase is the token the Run now sits in; required, one of ValidRunPhases.
	ToPhase string
	// TS is when the transition was observed. A zero value defers to the DB
	// default (now()) — the caller normally supplies the CR's observed time.
	TS time.Time
}

// RecordRunPhaseTransition appends one Run phase edge to the append-only journal
// (coord.run_phase_transition). It is the SANCTIONED write path — a plain INSERT
// into the trigger-guarded table; there is deliberately no update/delete path.
//
// Semantics:
//
//   - (nil): the row was appended.
//
//   - (ErrInvalidRunPhaseTransition): empty runUID/projectID/toPhase, or a phase
//     token outside ValidRunPhases (fail-closed, BEFORE any DB round-trip).
//
//   - (err): infrastructure failure (incl. a non-uuid id tripping 22P02);
//     nothing was written.
//
//   - (ErrRunPhaseEdgeSuppressed): the backward-edge guard (ISI-7541) rejected
//     the edge — it regresses the band, or re-enters the band the journal
//     already holds, and the durable claim fence was NOT bumped since the last
//     journaled edge. Nothing was written; this is NOT an infrastructure
//     failure, and adapters may log-and-swallow it.
//
// Idempotency is the EMITTER's job (WS-2 emits only on an actual phase change) —
// the journal is append-only and does not dedupe, exactly like audit_log.
func RecordRunPhaseTransition(ctx context.Context, db *sql.DB, row RunPhaseTransition) error {
	if db == nil {
		return errors.New("coord.RecordRunPhaseTransition: nil db")
	}
	if row.RunUID == "" || row.ProjectID == "" || row.ToPhase == "" {
		return fmt.Errorf("%w: runUID, projectID and toPhase are required", ErrInvalidRunPhaseTransition)
	}
	if !ValidRunPhases[row.ToPhase] {
		return fmt.Errorf("%w: toPhase %q not in (claiming, running, complete, failed)", ErrInvalidRunPhaseTransition, row.ToPhase)
	}
	if row.FromPhase != "" && !ValidRunPhases[row.FromPhase] {
		return fmt.Errorf("%w: fromPhase %q not in (claiming, running, complete, failed)", ErrInvalidRunPhaseTransition, row.FromPhase)
	}

	teamParam := sql.NullString{}
	if row.TeamID != "" {
		teamParam = sql.NullString{String: row.TeamID, Valid: true}
	}
	fromParam := sql.NullString{}
	if row.FromPhase != "" {
		fromParam = sql.NullString{String: row.FromPhase, Valid: true}
	}
	tsParam := sql.NullTime{}
	if !row.TS.IsZero() {
		tsParam = sql.NullTime{Time: row.TS, Valid: true}
	}

	// ── Backward-edge guard (ISI-7541) ── the journal's own last edge is the
	// authority, NOT the caller's FromPhase: the crash path corrupts the Run CR
	// the projector derived that from (ISI-7538: the CR flapped
	// claiming→running→claiming and ended Pending while the journal already held
	// running). Both reads are plain SELECTs — this stays observability history,
	// not a custody surface (the 0037 discipline).
	var lastTo sql.NullString
	var lastFence sql.NullInt64
	switch err := db.QueryRowContext(ctx,
		`SELECT to_phase, fence FROM coord.run_phase_transition
			 WHERE run_uid = $1::uuid ORDER BY id DESC LIMIT 1`,
		row.RunUID).Scan(&lastTo, &lastFence); {
	case errors.Is(err, sql.ErrNoRows):
		// No journal history for this run: the edge is forward by definition.
	case err != nil:
		return fmt.Errorf("coord.RecordRunPhaseTransition: read last edge of run %s: %w", row.RunUID, err)
	}

	// The legitimacy oracle: coord.claim.fence_token for the Run's work item.
	// Every genuine retry lap re-acquires the claim and bumps it (§6.2 acquire,
	// §6.4 re-acquire, cancel-release); the crash-path re-drive of a still-live
	// claim keeps the SAME fence (RedriveClaim's no-op fast path). An unresolvable
	// ref (empty, malformed, or no claim row yet) yields NULL — an honest "cannot
	// certify", which the decision below treats fail-closed.
	var fence sql.NullInt64
	if id, err := uuid.Parse(row.WorkItemID); err == nil {
		switch err := db.QueryRowContext(ctx,
			`SELECT fence_token FROM coord.claim WHERE work_item_id = $1::uuid`,
			id).Scan(&fence); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("coord.RecordRunPhaseTransition: read fence of work item %s: %w", row.WorkItemID, err)
		}
	}
	if lastTo.Valid {
		bumped := fence.Valid && lastFence.Valid && fence.Int64 > lastFence.Int64
		if (runPhaseRank(row.ToPhase) < runPhaseRank(lastTo.String) || row.ToPhase == lastTo.String) && !bumped {
			return fmt.Errorf("%w: run %s edge %q->%q vs last journaled %q (fence now %v, last %v)",
				ErrRunPhaseEdgeSuppressed, row.RunUID, row.FromPhase, row.ToPhase, lastTo.String,
				nullFence(fence), nullFence(lastFence))
		}
	}

	// ts = COALESCE(param, now()): a zero TS defers to the DB default so the row
	// still carries an honest observation time. fence stamps the oracle reading
	// (0043) so the NEXT backward edge for this run can compare against it.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO coord.run_phase_transition (run_uid, team_id, project_id, from_phase, to_phase, ts, fence)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, COALESCE($6::timestamptz, now()), $7::bigint)`,
		row.RunUID, teamParam, row.ProjectID, fromParam, row.ToPhase, tsParam, fence); err != nil {
		return fmt.Errorf("coord.RecordRunPhaseTransition: insert run %s edge %q->%q: %w",
			row.RunUID, row.FromPhase, row.ToPhase, err)
	}
	return nil
}

// RetryReclaimSinceLastPhaseEdge reports whether a §6.3/§8 retry re-entry was
// committed for the run strictly AFTER the run's last journaled phase edge
// (ISI-7539). RetryEnter co-commits its fence bump with a retry_lap_entered
// audit row, so the audit trail IS the durable record of every legitimate
// backward-edge producer; the journal's own last row bounds the window, making
// the verdict restart-safe (no adapter memory involved). On the crash-path flap
// from the ISI-7538 repro the racing forward row lands after the reclaim's
// audit row, so the window contains no reclaim and the flap's backward edge is
// suppressible. A run with no journaled edge yet bounds against -infinity: any
// retry reclaim on record legitimizes, and a first-life run has none — its
// backward edge is a re-observation artifact.
//
// Semantics:
//   - (nil): the oracle answered; true means a reclaim backs a backward edge.
//   - (ErrInvalidRunPhaseTransition): empty runUID (fail-closed, before any DB
//     round-trip — same input discipline as the write seam).
//   - (err): infrastructure failure; the CALLER fails open (the journal is
//     observability history and must not silently swallow an honest regression).
func RetryReclaimSinceLastPhaseEdge(ctx context.Context, db *sql.DB, runUID string) (bool, error) {
	if db == nil {
		return false, errors.New("coord.RetryReclaimSinceLastPhaseEdge: nil db")
	}
	if runUID == "" {
		return false, fmt.Errorf("%w: runUID is required", ErrInvalidRunPhaseTransition)
	}
	var reclaimed bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM coord.audit_log a
			 WHERE a.run_id = $1::uuid
			   AND a.event_type = 'retry_lap_entered'
			   AND a.created_at > (SELECT COALESCE(MAX(t.ts), '-infinity'::timestamptz)
			                         FROM coord.run_phase_transition t
			                        WHERE t.run_uid = $1::uuid)
		)`, runUID).Scan(&reclaimed); err != nil {
		return false, fmt.Errorf("coord.RetryReclaimSinceLastPhaseEdge: run %s: %w", runUID, err)
	}
	return reclaimed, nil
}

// nullFence renders a nullable fence for the suppression message ("unknown"
// reads better than "{0 false}" in operator logs).
func nullFence(f sql.NullInt64) interface{} {
	if f.Valid {
		return f.Int64
	}
	return "unknown"
}
