// agentdispatch.go — the AGENT-facing half of the board dispatch (ADR-0024 §5,
// ISI-4741 / origin ISI-4711). Where RequestDispatch (workitemdispatch.go) is the
// HUMAN assign a board user drives, AgentRequestDispatch is the PM→implementer
// handoff a capability-holding agent drives: "I hold this epic; hand this child to
// implementer X." It adds exactly ONE gate the human path does not — the caller
// must hold the item in custody (or an ancestor of it, O-3) — and then delegates
// to the single-sourced RequestDispatch, so the agent-∈-Team authority, the
// backlog→todo CAS, the re-assign branch and the §6.5 audit all live in exactly
// one place. The only provenance difference is Initiator="agent": the audit row is
// honestly stamped as an agent-initiated dispatch, never spoofed as a human one.
//
// Custody is checked with the SAME agentHoldsCustodyScope walk the create/update
// lane uses (workitemauthor.go), so "in custody" means one thing across the whole
// authoring surface. A target the agent does not hold — whether absent or simply
// unheld — is refused with ErrAgentAuthorNotInCustody (existence-hiding, §12.1): an
// agent cannot probe for the existence of work outside its custody. The target-∈-
// Team check that the human path already performs (via the TeamAgentResolver) is
// inherited unchanged, so an agent still cannot hand work to an agent outside the
// item's Team.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AgentRequestDispatchInput is one agent-driven PM→implementer handoff. Identity
// (Principal / AgentName / RunID / TeamID) is the caller's SERVER-STAMPED session
// scope, never a tool argument — the MCP edge fills it from the session headers
// (X-Principal-Id / X-Agent-Id / X-Run-Id / X-Team-Id, WINV2). WorkItemID is the
// item the agent holds in custody; AssigneeAgentID is the implementer it is handed
// to (matched against the item's Team by the inherited resolver guard).
type AgentRequestDispatchInput struct {
	WorkItemID      string
	AssigneeAgentID string
	Principal       string
	AgentName       string
	RunID           string
	TeamID          string
}

// AgentRequestDispatch verifies the calling agent holds WorkItemID in its custody
// scope, then delegates to RequestDispatch (Initiator="agent"). Custody is the only
// net-new gate; everything else — agent-∈-Team, unclaimed backlog/todo precondition,
// CAS, audit — is inherited from the human dispatch path.
//
// Semantics:
//   - (result, nil): the item advanced to 'todo' with AssigneeAgentID as requested
//     agent (or a re-assign was recorded), audit stamped initiator=agent.
//   - (zero, ErrInvalidWorkItem): missing identity, work item id or assignee.
//   - (zero, ErrAgentAuthorNotInCustody): the caller holds neither the item nor any
//     ancestor — returned identically whether the item is missing or unheld
//     (existence-hiding), so an agent cannot probe outside its custody.
//   - (zero, err): any RequestDispatch error (ErrWorkItemNotFound / ErrAgentNotInTeam
//     / ErrStateConflict / infrastructure), surfaced unchanged.
func (s *WorkItemDispatchStore) AgentRequestDispatch(ctx context.Context, in AgentRequestDispatchInput) (WorkItemDispatchResult, error) {
	if in.Principal == "" || in.AgentName == "" || in.RunID == "" {
		return WorkItemDispatchResult{}, fmt.Errorf("%w: principal, agentName and runId are required for agent dispatch", ErrInvalidWorkItem)
	}
	if in.WorkItemID == "" || in.AssigneeAgentID == "" {
		return WorkItemDispatchResult{}, fmt.Errorf("%w: workItemId and assigneeAgentId are required", ErrInvalidWorkItem)
	}

	// Custody gate (the only net-new check): the caller must hold the item or an
	// ancestor. Read in its own short transaction; RequestDispatch re-validates the
	// item's state under its own FOR UPDATE / CAS, so a benign race between the two
	// is caught there (ErrStateConflict), never a lost update.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WorkItemDispatchResult{}, fmt.Errorf("coord.AgentRequestDispatch: begin: %w", err)
	}
	held, err := agentHoldsCustodyScope(ctx, tx, in.WorkItemID, in.Principal, in.AgentName)
	_ = tx.Rollback() // read-only; nothing to commit.
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkItemDispatchResult{}, ErrAgentAuthorNotInCustody // existence-hiding
		}
		return WorkItemDispatchResult{}, err
	}
	if !held {
		return WorkItemDispatchResult{}, ErrAgentAuthorNotInCustody
	}

	// Delegate to the single-sourced human dispatch, pinned to the caller's Team
	// (so the inherited agent-∈-Team guard fences the assignee) and stamped as an
	// agent-initiated handoff.
	return s.RequestDispatch(ctx, RequestDispatchInput{
		WorkItemID: in.WorkItemID,
		AgentID:    in.AssigneeAgentID,
		TeamID:     in.TeamID,
		Principal:  in.Principal,
		Initiator:  "agent",
	})
}
