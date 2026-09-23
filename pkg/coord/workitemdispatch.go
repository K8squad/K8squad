// workitemdispatch.go — the board's assign-agent → start-Run custody op
// (ADR-0022 §3 D3/D4/D5, ISI-4411 / ISI-4400 Part B), the third human write on
// coord.work_item beside the lane move (humanstate.go) and create/edit
// (workitemwrite.go).
//
// WHAT "start a Run" ACTUALLY IS (ADR-0022 §1.2): NOT a synchronous poke to the
// operator. A Run mints out-of-band — the operator Intake sweep scans
// coord.work_item WHERE state='todo' and creates the Run CR, hardcoding the agent
// to Team.Spec.Agents[0] (intake.go:287-314). So "dispatch this ticket to agent
// X" decomposes into two durable facts written in ONE txn:
//  1. the human's agent choice — requested_agent (mig 0021), the pre-run INTENT
//     Intake prefers over the hardcoded Team.Spec.Agents[0];
//  2. the lane advance backlog→todo, which is what makes Intake pick it up.
//
// No new dispatch spine; the Run still starts because the item is now in todo.
//
// AUTHORIZATION (§3 D4): this is the ONE place the agent-∈-Team gap is closed.
// Admission (validator.go) only checks that a Run's agent EXISTS, never that it
// belongs to the owning Team — a guarantee that held implicitly only because
// Intake always took Team.Spec.Agents[0]. The moment the board can pick an
// arbitrary agent that implicit guarantee is gone, so RequestDispatch enforces
// membership explicitly (via the TeamAgentResolver seam) inside the same txn that
// advances the lane: a non-member never advances the item.
//
// FAILURE SEMANTICS (§3 D5): create and dispatch are independent calls with no
// distributed txn. Every refusal here (bad input, tenancy, agent-not-in-team,
// wrong lane) rolls the txn back, leaving the item exactly as it was — for a
// fresh ticket that is "still in backlog, unassigned." The write is all-or-none.
//
// No fence (ADR-037): like the lane move, a human dispatch decision holds no
// custody of the item, so the audit row carries fence_token NULL.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrAgentNotInTeam — the requested agent is not a member of the owning Team's
// composition (Team.Spec.Agents). This is the §3 D4 authorization check that
// admission does not cover; the HTTP shell maps it to 403.
var ErrAgentNotInTeam = errors.New("coord: requested agent is not a member of the owning team")

// TeamAgentResolver lists a Team's agent composition (the names in
// Team.Spec.Agents) by the Team CR uid. That composition lives in the Team CR
// (Kubernetes), not the coord schema, so coord stays storage-pure via this seam:
// the apiserver injects an implementation backed by the shared informer cache
// (the same reader the board's Project resolver uses). A uid that resolves to no
// Team CR is an ERROR (a dangling team), never a silent empty set — dispatch must
// fail loudly rather than vacuously reject every agent as "not a member."
type TeamAgentResolver interface {
	TeamAgents(ctx context.Context, teamUID string) ([]string, error)
}

// RequestDispatchInput is one board dispatch. WorkItemID, AgentID and Principal
// are required. TeamID is the caller's server-derived Team scope (never trusted
// from the request body): "" for a trusted/fleet caller, else the item must
// belong to it or the op is 404 (existence-hiding, §12.1). InitiatedByUserID is
// the §12.4 on-behalf-of id.
type RequestDispatchInput struct {
	WorkItemID        string
	AgentID           string
	TeamID            string
	Principal         string
	InitiatedByUserID string
	// Initiator names who requested the dispatch, stamped verbatim into the §6.5
	// audit payload. Empty ⇒ "human" (the board default) so every existing caller
	// and audit row is byte-identical; the agent authoring lane (ADR-0024,
	// AgentRequestDispatch) passes "agent" so a PM→implementer handoff is
	// provenanced as agent-initiated, never spoofed as a human dispatch.
	Initiator string
}

// WorkItemDispatchResult is the outcome of one successful dispatch: the lane advance the
// board renders from, plus the agent the human chose (now durable intent Intake
// will honor).
type WorkItemDispatchResult struct {
	WorkItemID     string `json:"workItemId"`
	FromState      string `json:"fromState"`
	ToState        string `json:"toState"`
	RequestedAgent string `json:"requestedAgent"`
}

// WorkItemDispatchStore executes the board dispatch op against the shipped coord
// schema (0001 + 0021). It holds the *sql.DB and the Team-composition resolver
// (the agent-∈-Team authority); no other mutable state, so its method is safe for
// concurrent use (each dispatch opens its own transaction).
type WorkItemDispatchStore struct {
	db     *sql.DB
	agents TeamAgentResolver
}

// NewWorkItemDispatchStore binds the dispatch op to db and the Team-composition
// resolver. Both are required — the resolver is the §3 D4 authorization source,
// so a nil one is a construction error, never a silently-skipped check.
func NewWorkItemDispatchStore(db *sql.DB, agents TeamAgentResolver) (*WorkItemDispatchStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewWorkItemDispatchStore: nil db")
	}
	if agents == nil {
		return nil, errors.New("coord.NewWorkItemDispatchStore: nil team-agent resolver")
	}
	return &WorkItemDispatchStore{db: db, agents: agents}, nil
}

// RequestDispatch records a human's "run agent X on this ticket" as durable
// intent (requested_agent) and advances the lane backlog→todo so the operator
// Intake sweep mints the Run — atomically, with a §6.5 'work_item_dispatch_requested'
// audit row (fence NULL, ADR-037), and only after the agent-∈-Team check passes.
//
// RE-ASSIGN (ISI-4573): the same verb re-targets an item already in 'todo' that
// no run has claimed yet — requested_agent is swapped in place (no lane move)
// with a 'work_item_reassign_requested' audit row, same guards, same txn shape.
//
// RE-RUN (ISI-4808): the same verb also re-dispatches an already-worked ticket
// parked off the dispatch lanes — a working/engine lane (the six phases +
// in_progress + in_review) whose checkout is RELEASED, or a terminal
// (done/cancelled) — by stamping requested_agent and advancing that lane → 'todo'
// with a 'work_item_rerun_requested' audit row. The idempotency guard is
// LIVENESS, not lane (§3 D5): only a LIVE run (coord.claim.holder_principal set)
// is refused, so a parked/settled item re-runs without the human having to move
// the card back. This unifies with the comment nudge (workitemcomment.go,
// ISI-4495), which shares the working/engine lane set and the same holder guard
// but does NOT record an agent and does NOT reopen terminals.
//
// Semantics:
//   - (result, nil): requested_agent stamped and the item now in 'todo' (fresh
//     backlog dispatch, or a re-run from a parked working/engine/terminal lane —
//     fromState is the lane we left), or requested_agent swapped on an unclaimed
//     'todo' item (re-assign, fromState==toState=="todo"); Intake will prefer
//     AgentID over Team.Spec.Agents[0] on its next tick.
//   - (zero, ErrInvalidWorkItem): missing required input, or the item has no
//     owning Team to check membership against (400).
//   - (zero, ErrWorkItemNotFound): no such item in the caller's Team scope (404,
//     existence-hiding — never a cross-tenant 403).
//   - (zero, ErrAgentNotInTeam): the agent is not in the owning Team's composition
//     (403). The item is left untouched (§3 D5).
//   - (zero, ErrStateConflict): a claimed todo (coord.claim.run_id set) or a lane
//     held by a LIVE run (holder_principal set) — a clean 409 naming the state,
//     never a silent second start behind a live run (§3 D5 idempotency) — or an
//     unknown/undispatchable lane (fail closed).
//   - (zero, err): infrastructure failure (incl. a dangling team the resolver
//     cannot resolve); nothing was written.
func (s *WorkItemDispatchStore) RequestDispatch(ctx context.Context, in RequestDispatchInput) (WorkItemDispatchResult, error) {
	if in.WorkItemID == "" || in.AgentID == "" || in.Principal == "" {
		return WorkItemDispatchResult{}, fmt.Errorf("%w: workItemID, agentId and principal are required", ErrInvalidWorkItem)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItemDispatchResult{}, fmt.Errorf("coord.RequestDispatch: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// (1) Lock + read the target's lane and owning Team. FOR UPDATE serialises a
	// racing lane move / edit / re-dispatch against us so the membership check and
	// the CAS advance act on one consistent row.
	var currentState string
	var itemTeam sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT state, team_id FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`,
		in.WorkItemID).Scan(&currentState, &itemTeam)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WorkItemDispatchResult{}, ErrWorkItemNotFound
	case err != nil:
		return WorkItemDispatchResult{}, fmt.Errorf("coord.RequestDispatch: read current: %w", err)
	}

	// (2) Tenancy (§12.1): an item outside the caller's Team is 404, not 403.
	if in.TeamID != "" && (!itemTeam.Valid || itemTeam.String != in.TeamID) {
		return WorkItemDispatchResult{}, ErrWorkItemNotFound
	}

	// (3) A team-less item has no composition to authorize against and no squad for
	// Intake to dispatch to (intake.go requires team_id NOT NULL) — reject 400
	// rather than write intent that can never mint a Run.
	if !itemTeam.Valid || itemTeam.String == "" {
		return WorkItemDispatchResult{}, fmt.Errorf("%w: work item has no owning team to dispatch to", ErrInvalidWorkItem)
	}

	// (4) Branch on the LOCKED lane, still inside the txn: backlog is the dispatch
	// advance below, unchanged; todo is the re-assign window iff no run has claimed
	// the item yet (ISI-4573); a parked working/engine lane OR a terminal is the
	// ISI-4808 RE-RUN window iff no LIVE run holds the checkout; a claimed todo or
	// a lane held by a live run is past this door (§3 D5).
	//
	// ISI-4808 — the D5 idempotency guard is LIVENESS, not lane. ADR-0022 §3 D5
	// says "never a silent second start behind a LIVE run"; the original code
	// enforced that by refusing every lane past unclaimed-todo, which also refused
	// PARKED runs (a settled/awaiting-review item whose checkout is released) that
	// are exactly what a human wants to re-run. That over-broad lane gate is why
	// users had to move the card back to force a re-run. The honest guard is the
	// one the comment re-trigger already uses (workitemcomment.go, ISI-4495):
	// coord.claim.holder_principal — a live checkout means a run is actively
	// working, and only THAT must be refused. So the assign verb now unifies with
	// the comment nudge: same lane set, same liveness guard, but the verb ALSO
	// records the chosen agent (requested_agent) so "comment + assign" is one
	// reliable path, not two racing ones. Terminals (done/cancelled) hold no live
	// run, so the deliberate assign verb reopens+dispatches them too — the comment
	// nudge deliberately does NOT (conversation must not resurrect closed work),
	// which is why the two paths share the working/engine lanes but only the verb
	// extends to terminals.
	var fromState, toState, eventType, updateSQL string
	// updateArgs are the CAS write's bind values; the re-run branch appends the
	// locked lane as $3 so currentState is never interpolated into the SQL text.
	updateArgs := []any{in.WorkItemID, in.AgentID}
	switch {
	case currentState == "backlog":
		fromState, toState, eventType = "backlog", "todo", "work_item_dispatch_requested"
		updateSQL = `
			UPDATE coord.work_item
			   SET requested_agent = $2, state = 'todo', updated_at = now()
			 WHERE id = $1::uuid AND state = 'backlog'`
	case currentState == "todo":
		// The re-assign precondition: coord.claim must carry no run for this item.
		// The claim row is locked FOR UPDATE in the same txn so a run claiming
		// concurrently (prodclaim.go rewrites run_id) cannot interleave with this
		// check — either the claim commits first and we see its run_id (409), or
		// we commit first and the claimer re-reads the swapped intent. (The lock
		// order work_item→claim is the inverse of prodclaim's claim→work_item, so
		// a truly simultaneous pair is resolved by Postgres' deadlock detector as
		// a retryable infra error, never a corrupted re-assign.)
		var runID sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT run_id FROM coord.claim WHERE work_item_id = $1::uuid FOR UPDATE`,
			in.WorkItemID).Scan(&runID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The shipped schema provisions exactly one claim row per item (mig
			// 0001 trigger); a missing row cannot hold a run, so it reads
			// unclaimed.
		case err != nil:
			return WorkItemDispatchResult{}, fmt.Errorf("coord.RequestDispatch: read claim: %w", err)
		case runID.Valid:
			return WorkItemDispatchResult{}, fmt.Errorf("%w: item is in %q with a claimed run; re-assign requires an unclaimed todo item", ErrStateConflict, currentState)
		}
		fromState, toState, eventType = "todo", "todo", "work_item_reassign_requested"
		updateSQL = `
			UPDATE coord.work_item
			   SET requested_agent = $2, updated_at = now()
			 WHERE id = $1::uuid AND state = 'todo'`
	case commentReTriggerLanes[currentState] || currentState == "done" || currentState == "cancelled":
		// ISI-4808 RE-RUN: an already-worked ticket parked off the dispatch lanes.
		// The working/engine lanes (commentReTriggerLanes — the six phases +
		// in_progress + in_review) re-run iff NO live run holds the checkout, the
		// exact liveness guard the comment nudge uses. Terminals (done/cancelled)
		// hold no live run by construction, so the deliberate verb reopens them.
		//
		// The live-run guard: read coord.claim.holder_principal FOR UPDATE (locking
		// the claim row the same way the todo branch locks run_id) so a run
		// acquiring concurrently (prodclaim.go writes holder_principal + advances
		// the lane) cannot interleave with this check. A live holder → 409 pointing
		// at the Kill + re-dispatch flow; an absent holder → re-enter todo.
		var holder sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT holder_principal FROM coord.claim WHERE work_item_id = $1::uuid FOR UPDATE`,
			in.WorkItemID).Scan(&holder)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// No claim row ⇒ no holder ⇒ safe to re-run (mig 0001 provisions one
			// row per item, but a terminal item may predate it; treat as unheld).
		case err != nil:
			return WorkItemDispatchResult{}, fmt.Errorf("coord.RequestDispatch: read claim holder: %w", err)
		case holder.Valid:
			return WorkItemDispatchResult{}, fmt.Errorf("%w: item is in %q with a live run (%s); re-run requires killing the current run first", ErrStateConflict, currentState, holder.String)
		}
		fromState, toState, eventType = currentState, "todo", "work_item_rerun_requested"
		updateArgs = append(updateArgs, currentState) // $3 — the locked lane, CAS-re-asserted
		// CAS re-asserts BOTH the lane we locked ($3) AND holder-still-absent, so a
		// run that claimed between our read and this write fails the CAS (0 rows →
		// ErrStateConflict below) and never has its live lane clobbered.
		updateSQL = `
			UPDATE coord.work_item
			   SET requested_agent = $2, state = 'todo', updated_at = now()
			 WHERE id = $1::uuid
			   AND state = $3
			   AND NOT EXISTS (
			       SELECT 1 FROM coord.claim c
			        WHERE c.work_item_id = coord.work_item.id
			          AND c.holder_principal IS NOT NULL)`
	default:
		// Unknown/unhandled lane — fail closed, never guess a re-run.
		return WorkItemDispatchResult{}, fmt.Errorf("%w: item is in %q, which is not a dispatchable lane", ErrStateConflict, currentState)
	}

	// (5) Authorization the admission layer does NOT cover (§3 D4): the agent must
	// belong to the owning Team's composition. Checked inside the txn so a
	// non-member can never advance the lane OR swap re-assign intent. A dangling
	// team (resolver error) fails the whole op loudly rather than rejecting every
	// agent vacuously.
	names, err := s.agents.TeamAgents(ctx, itemTeam.String)
	if err != nil {
		return WorkItemDispatchResult{}, fmt.Errorf("coord.RequestDispatch: resolve team agents: %w", err)
	}
	if !containsString(names, in.AgentID) {
		return WorkItemDispatchResult{}, fmt.Errorf("%w: agent %q not in team %s", ErrAgentNotInTeam, in.AgentID, itemTeam.String)
	}

	// (6) Conditional write: every branch CAS's on the lane the lock read saw, so a
	// slipped concurrent change is a conflict, never a silent clobber.
	res, err := tx.ExecContext(ctx, updateSQL, updateArgs...)
	if err != nil {
		return WorkItemDispatchResult{}, fmt.Errorf("coord.RequestDispatch: update: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return WorkItemDispatchResult{}, fmt.Errorf("coord.RequestDispatch: rows: %w", err)
	} else if n == 0 {
		return WorkItemDispatchResult{}, fmt.Errorf("%w: concurrent lane change", ErrStateConflict)
	}

	// (7) §6.5 audit provenance, same txn, fence NULL (ADR-037: a dispatch holds no
	// custody). Mirrors humanstate.go's state_transition row shape so the whole
	// board write surface reads one way; the re-assign branch books its own event
	// type with from==to=="todo" (the lane did not move).
	initiator := in.Initiator
	if initiator == "" {
		initiator = "human" // board default; keeps every human dispatch audit byte-identical.
	}
	payload, err := json.Marshal(map[string]any{
		"initiator":       initiator,
		"requested_agent": in.AgentID,
	})
	if err != nil {
		return WorkItemDispatchResult{}, fmt.Errorf("coord.RequestDispatch: audit payload: %w", err)
	}
	var initiatedBy sql.NullString
	if in.InitiatedByUserID != "" {
		initiatedBy = sql.NullString{String: in.InitiatedByUserID, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, event_type, principal, initiated_by_user_id, from_state, to_state, payload)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::jsonb)`,
		in.WorkItemID, eventType, in.Principal, initiatedBy, fromState, toState, string(payload)); err != nil {
		return WorkItemDispatchResult{}, fmt.Errorf("coord.RequestDispatch: audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return WorkItemDispatchResult{}, fmt.Errorf("coord.RequestDispatch: commit: %w", err)
	}
	return WorkItemDispatchResult{
		WorkItemID:     in.WorkItemID,
		FromState:      fromState,
		ToState:        toState,
		RequestedAgent: in.AgentID,
	}, nil
}

// containsString reports whether want is in set (the Team composition). Exact
// match on the agent name — the same identity Intake dispatches on (intake.go:290).
func containsString(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}
