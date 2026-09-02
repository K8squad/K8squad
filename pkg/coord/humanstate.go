// humanstate.go — Story 8.14a / ISI-2909 (gap ISI-2876): the HUMAN board-lane
// status-transition custody op against the shipped coord schema (0001).
//
// The Kanban board is a PROJECTION of coord.work_item.state (§13 board-derivation,
// §8.6); there was a state enum + projection but no write path for a human to move
// an item between lanes. This is that write path, kept on the custody side of the
// house so the HTTP surface (internal/apiserver) stays a thin auth+mapping shell.
//
// It is NOT a claim/lease operation: a human moving a card holds no custody of the
// item and bumps no fence. So — "no-fence per ADR-037" — the audit row is written
// with fence_token NULL (exactly like the coordinator's dispatch/reroute DECISIONS,
// which also act by reading the record rather than holding it, §6.2/§2.9) and the
// coord.claim fence is never touched. A concurrent agent lease is therefore neither
// created, observed, nor invalidated by a lane move.
//
// `blocked` is deliberately absent from the transition set: it is an orthogonal
// condition (blocked_reason), never a lane (§8.6), so it is not a target state here.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// humanStates is the canonical board-lane enum a human may target — the exact set
// the 0001 work_item.state CHECK constraint pins (§8.6). `blocked` is intentionally
// excluded: it is a condition (blocked_reason), not a lane.
var humanStates = map[string]bool{
	"backlog":     true,
	"todo":        true,
	"in_progress": true,
	"in_review":   true,
	"done":        true,
}

// StateTransition is the outcome of one human lane move: the lane the item left and
// the lane it now sits in (custody-free projection, §8.6). Read-only value.
type StateTransition struct {
	WorkItemID string `json:"workItemId"`
	FromState  string `json:"fromState"`
	ToState    string `json:"toState"`
}

// Sentinel errors so the HTTP shell can map custody outcomes to status codes without
// re-implementing the rules. Refusals leave the record unchanged (txn rolled back).
var (
	// ErrInvalidState — the target lane is not one of the §8.6 board lanes (→ 400).
	ErrInvalidState = errors.New("coord: invalid target state (not a board lane)")
	// ErrWorkItemNotFound — no such item in the caller's Team scope (→ 404). Mapped to
	// 404-not-403 so a cross-tenant probe cannot distinguish "absent" from "forbidden".
	ErrWorkItemNotFound = errors.New("coord: work item not found")
	// ErrStateConflict — the item's current lane does not satisfy the transition's
	// precondition: either an explicit fromState guard missed, or the item is already
	// in the target lane (→ 409). Optimistic-concurrency guard for the board.
	ErrStateConflict = errors.New("coord: state transition conflict")
)

// HumanStateStore executes the human board-lane transition against the shipped coord
// schema (0001). It holds no mutable Go state beyond the *sql.DB, so its method is
// safe for concurrent use by many goroutines (each transition opens its own txn).
type HumanStateStore struct {
	db *sql.DB
}

// NewHumanStateStore binds the human state-transition op to db.
func NewHumanStateStore(db *sql.DB) (*HumanStateStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewHumanStateStore: nil db")
	}
	return &HumanStateStore{db: db}, nil
}

// TransitionState moves one work item to targetState on behalf of a human principal,
// atomically, with §6.5 audit provenance and NO fence (ADR-037: a lane move is not a
// custody operation).
//
// teamID scopes tenancy (§12.1): an item outside the caller's Team is invisible —
// ErrWorkItemNotFound (404), never a cross-tenant 403. Pass "" only for a trusted,
// already-tenancy-checked caller (there is none on the HTTP path).
//
// fromState, when non-empty, is an optimistic-concurrency precondition: the move is
// refused with ErrStateConflict unless the item's current lane equals it. This lets
// the board send the lane it rendered from and fail closed on a racing change.
//
// Semantics:
//   - (transition, nil): the item is now in targetState; from_state→to_state and the
//     principal are recorded in coord.audit_log (event_type='state_transition',
//     fence_token NULL) in the same transaction.
//   - (zero, ErrInvalidState): targetState / fromState is not a board lane (400).
//   - (zero, ErrWorkItemNotFound): no such item in the Team scope (404).
//   - (zero, ErrStateConflict): fromState guard missed, or the item is already in
//     targetState — no lane change to make (409).
//   - (zero, err): infrastructure failure; nothing was written.
func (s *HumanStateStore) TransitionState(ctx context.Context, workItemID, teamID, targetState, fromState, principal, initiatedByUserID string) (StateTransition, error) {
	return s.transition(ctx, workItemID, teamID, targetState, fromState, principal, "human", initiatedByUserID)
}

// AgentTransitionState is the AGENT-initiated counterpart of TransitionState
// (ISI-3601 S2 update-status): a run moves its OWN work item to targetState.
// Tenancy is already enforced upstream by the run-scoped task-io token binding
// (own-run only), so teamID is "" (trusted caller) and there is no on-behalf-of
// user. The ONLY difference from a human move is the audit provenance —
// initiator="agent", not "human" — so the §6.5 audit_log truthfully attributes
// the transition. The same state enum, optimistic fromState guard, no-fence
// (ADR-037) and conflict/not-found semantics apply.
func (s *HumanStateStore) AgentTransitionState(ctx context.Context, workItemID, targetState, fromState, principal string) (StateTransition, error) {
	return s.transition(ctx, workItemID, "", targetState, fromState, principal, "agent", "")
}

func (s *HumanStateStore) transition(ctx context.Context, workItemID, teamID, targetState, fromState, principal, initiator, initiatedByUserID string) (StateTransition, error) {
	if workItemID == "" || targetState == "" || principal == "" {
		return StateTransition{}, fmt.Errorf(
			"coord.HumanStateStore.TransitionState: workItemID, targetState and principal are required "+
				"(got item=%q target=%q principal=%q)", workItemID, targetState, principal)
	}
	if !humanStates[targetState] {
		return StateTransition{}, fmt.Errorf("%w: %q", ErrInvalidState, targetState)
	}
	if fromState != "" && !humanStates[fromState] {
		return StateTransition{}, fmt.Errorf("%w: fromState %q", ErrInvalidState, fromState)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StateTransition{}, fmt.Errorf("coord.HumanStateStore.TransitionState: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// (1) Lock the row and read its current lane + Team. FOR UPDATE serialises a
	// concurrent human move (or an agent state change) against us so the guard below
	// and the UPDATE act on one consistent lane, not a torn read-then-write.
	var currentState string
	var itemTeam sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT state, team_id FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`,
		workItemID).Scan(&currentState, &itemTeam)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return StateTransition{}, ErrWorkItemNotFound
	case err != nil:
		return StateTransition{}, fmt.Errorf("coord.HumanStateStore.TransitionState: read current: %w", err)
	}

	// (2) Tenancy: an item outside the caller's Team is 404, not 403 (§12.1). A
	// scoped caller cannot move an item with a NULL/other Team.
	if teamID != "" && (!itemTeam.Valid || itemTeam.String != teamID) {
		return StateTransition{}, ErrWorkItemNotFound
	}

	// (3) Preconditions → 409. The explicit fromState guard, and the always-on
	// "there must be a lane to change" guard (already-in-target is a no-op, not a
	// transition) are both conflicts the caller should re-read and retry.
	if fromState != "" && currentState != fromState {
		return StateTransition{}, fmt.Errorf("%w: current lane %q, expected %q", ErrStateConflict, currentState, fromState)
	}
	if currentState == targetState {
		return StateTransition{}, fmt.Errorf("%w: item already in %q", ErrStateConflict, targetState)
	}

	// (4) Conditional UPDATE. The WHERE re-asserts the lane we locked so the write
	// is a strict compare-and-set even under the row lock (belt and braces).
	res, err := tx.ExecContext(ctx, `
		UPDATE coord.work_item
		   SET state = $2, updated_at = now()
		 WHERE id = $1::uuid AND state = $3`,
		workItemID, targetState, currentState)
	if err != nil {
		return StateTransition{}, fmt.Errorf("coord.HumanStateStore.TransitionState: update: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return StateTransition{}, fmt.Errorf("coord.HumanStateStore.TransitionState: rows: %w", err)
	} else if n == 0 {
		// The lane changed between our locked read and the UPDATE only if the row
		// lock were bypassed; treat a zero-row CAS as a conflict, never a silent OK.
		return StateTransition{}, fmt.Errorf("%w: concurrent lane change", ErrStateConflict)
	}

	// (5) §6.5 audit provenance. fence_token is NULL by omission (ADR-037: a human
	// lane move holds no custody). initiated_by_user_id is the §12.4 on-behalf-of id.
	payload, err := json.Marshal(map[string]any{
		"initiator":  initiator,
		"from_state": currentState,
		"to_state":   targetState,
	})
	if err != nil {
		return StateTransition{}, fmt.Errorf("coord.HumanStateStore.TransitionState: audit payload: %w", err)
	}
	var initiatedBy sql.NullString
	if initiatedByUserID != "" {
		initiatedBy = sql.NullString{String: initiatedByUserID, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, event_type, principal, initiated_by_user_id, from_state, to_state, payload)
		VALUES ($1::uuid, 'state_transition', $2, $3, $4, $5, $6::jsonb)`,
		workItemID, principal, initiatedBy, currentState, targetState, string(payload)); err != nil {
		return StateTransition{}, fmt.Errorf("coord.HumanStateStore.TransitionState: audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return StateTransition{}, fmt.Errorf("coord.HumanStateStore.TransitionState: commit: %w", err)
	}
	return StateTransition{WorkItemID: workItemID, FromState: currentState, ToState: targetState}, nil
}
