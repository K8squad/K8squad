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
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
//   - (nil): the row was appended.
//   - (ErrInvalidRunPhaseTransition): empty runUID/projectID/toPhase, or a phase
//     token outside ValidRunPhases (fail-closed, BEFORE any DB round-trip).
//   - (err): infrastructure failure (incl. a non-uuid id tripping 22P02);
//     nothing was written.
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

	// ts = COALESCE(param, now()): a zero TS defers to the DB default so the row
	// still carries an honest observation time.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO coord.run_phase_transition (run_uid, team_id, project_id, from_phase, to_phase, ts)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, COALESCE($6::timestamptz, now()))`,
		row.RunUID, teamParam, row.ProjectID, fromParam, row.ToPhase, tsParam); err != nil {
		return fmt.Errorf("coord.RecordRunPhaseTransition: insert run %s edge %q->%q: %w",
			row.RunUID, row.FromPhase, row.ToPhase, err)
	}
	return nil
}
