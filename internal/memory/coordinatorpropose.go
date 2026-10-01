package memory

// coordinatorpropose.go — the propose-mode half of the ADR-0024 agent authoring
// lane (ISI-5282, parent ISI-5267 §3.3 / WS-3).
//
// A coordinator Role may run in one of two autonomy modes (api.RoleSpec.CoordinatorMode):
//
//   - auto    (default) — the coordinator's work_item_create / work_item_assign
//     tool calls EXECUTE directly through the coord authoring seams (agentauthor.go),
//     exactly as before. Unchanged.
//   - propose           — the coordinator does NOT get to author directly: each
//     work_item_create / work_item_assign call instead RAISES an inert discussion
//     Proposal (kind='proposal') the human confirms. Execution then flows through
//     the EXISTING internal/apiserver/proposalconfirm.go fan-out after acceptance —
//     no new confirmation primitive, no new custody path.
//
// The mode is a SERVER-AUTHENTICATED grant, never a tool argument: the operator
// derives capability.CapabilityCoordinatorPropose ("coordinator.propose") for a
// coordinator Role whose CoordinatorMode is propose (pkg/capability.ResolveGrant),
// bakes it into the run token alongside work_item.author (pkg/controller/run
// assembly), and it arrives here in the session's capability set exactly like
// work_item.author. So a client cannot assert — or escape — propose mode.
//
// Deny-by-default is preserved: the propose gate bites AFTER requireAuthor (the
// work_item.author capability check), so a non-coordinator / ungranted agent is
// still refused upstream and never reaches this file. A propose-gated session with
// NO proposal channel wired (a deployment that could not build the resolver/poster)
// is refused honestly — it NEVER silently falls back to direct execute, which would
// defeat the human gate.

import (
	"context"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// CoordinatorProposeCapability is the grant slug the authoring edge reads to
// decide propose vs auto mode. It mirrors pkg/capability.CapabilityCoordinatorPropose
// (kept as a local const so this package does not import the operator capability
// package, matching WorkItemAuthorCapability's local mirror of the slug).
const CoordinatorProposeCapability = "coordinator.propose"

// sessionIsProposeCoordinator reports whether the server-authenticated session
// carries the coordinator.propose grant — the propose-mode signal. On the token
// (sandbox) path the capability set is the VERIFIED token claims; on the header
// (BFF) path it is the control-plane-stamped X-Agent-Capabilities. Either way it
// is un-spoofable by the tool caller.
func sessionIsProposeCoordinator(sess mcpSession) bool {
	for _, c := range sess.capabilities {
		if c == CoordinatorProposeCapability {
			return true
		}
	}
	return false
}

// ProposalThread is the discussion thread a propose-mode coordinator posts its
// proposal cards into — the conversation that invoked it. ProjectID is the room's
// "namespace/name" slug (ISI-3982), TeamID the room's tenancy, ThreadID the thread
// the card lands in. All three are resolved from the coordinator run's identity
// (never a tool argument), so a propose-mode coordinator cannot target a thread
// outside the room that dispatched it.
type ProposalThread struct {
	ProjectID string
	TeamID    uuid.UUID
	ThreadID  uuid.UUID
}

// ProposalThreadResolver resolves the discussion thread a coordinator Run was
// dispatched from, keyed by the run's server-authenticated id — the mention-dispatch
// ledger ride (runID → work_item → thread/project/team, internal/mentiondispatch).
// ok=false for any run that is NOT a room thread-run (e.g. a coordinator dispatched
// to a board ticket, not a room @-mention): there is no originating thread to post a
// proposal into, so the authoring tool refuses honestly rather than guessing a
// thread. A nil resolver (a deployment without the ledger) likewise leaves propose
// mode unwired. The signature is primitives-only so the ledger implementation
// (internal/mentiondispatch) satisfies it structurally without importing this
// package.
type ProposalThreadResolver interface {
	ThreadForDispatchedRun(ctx context.Context, runID string) (projectID string, teamID, threadID uuid.UUID, ok bool, err error)
}

// ProposalPoster posts one inert proposal card into a thread. *discussion.Store
// satisfies it via PostProposal — the SAME fenced store method the REST proposal
// path uses, so tenancy-scoping and the append-only/custody-free invariants live in
// the store, not here.
type ProposalPoster interface {
	PostProposal(ctx context.Context, projectID string, teamID, threadID uuid.UUID, auth discussion.AuthorContext, body string, payload discussion.ProposalPayload, parentID *uuid.UUID) (*discussion.Message, error)
}

// CoordinatorProposer turns a propose-mode coordinator's authoring tool call into
// an inert discussion Proposal. It is nil unless BOTH a thread resolver and a
// poster are wired (WithCoordinatorPropose); a nil proposer makes a propose-gated
// session refuse honestly at the edge (never execute directly).
type CoordinatorProposer struct {
	resolver ProposalThreadResolver
	poster   ProposalPoster
}

// NewCoordinatorProposer bundles the resolver + poster. It returns nil when either
// is absent, so the tool surface treats propose mode as unwired (the authoring edge
// then refuses a propose-gated call rather than silently executing it).
func NewCoordinatorProposer(resolver ProposalThreadResolver, poster ProposalPoster) *CoordinatorProposer {
	if resolver == nil || poster == nil {
		return nil
	}
	return &CoordinatorProposer{resolver: resolver, poster: poster}
}

// proposalToolResult is the tool result a propose-mode authoring call returns: the
// coordinator sees that it RAISED a proposal (not executed), and can report the
// proposal id in its completion summary. It deliberately mirrors the shape the
// direct-execute result carries an `action`, so a model reading either knows which
// board effect it drove / requested.
type proposalToolResult struct {
	Proposed   bool   `json:"proposed"`   // always true — this is the propose-mode result
	Action     string `json:"action"`     // create_ticket | assign_agent
	ProposalID string `json:"proposalId"` // the posted proposal message id (confirm/dismiss key)
	ThreadID   string `json:"threadId"`   // the room thread the card landed in
	Message    string `json:"message"`    // human-readable summary of what was proposed
	// AssigneeDeferred is set when a create call carried assignee_agent_id: a
	// create_ticket proposal cannot also assign (the ticket does not exist until the
	// human confirms the create), so the assignee is NOT proposed here. Raise a
	// separate assign_agent proposal against the created ticket after it is confirmed.
	AssigneeDeferred string `json:"assigneeDeferred,omitempty"`
}

// resolveThread is the shared thread-resolution + scoping step both propose paths
// run. It returns the thread the proposal must land in, or a tool error when the
// run has no originating room thread (the honest refusal — never a direct execute).
func (p *CoordinatorProposer) resolveThread(ctx context.Context, id authorIdentity) (ProposalThread, *string) {
	projectID, teamID, threadID, ok, err := p.resolver.ThreadForDispatchedRun(ctx, id.runID)
	if err != nil {
		msg := "propose mode: could not resolve this run's originating discussion thread: " + err.Error()
		return ProposalThread{}, &msg
	}
	if !ok {
		msg := "propose mode is enabled for this coordinator, but this run has no originating discussion thread to post a proposal into (it was not dispatched from a room @-mention). Raise the work in a room thread, or run the coordinator in auto mode."
		return ProposalThread{}, &msg
	}
	return ProposalThread{ProjectID: projectID, TeamID: teamID, ThreadID: threadID}, nil
}

// authorContext builds the proposal's author scope from the coordinator's
// server-authenticated identity. The proposal is agent-authored (inert): an agent
// may PROPOSE, never authorize its own proposal (proposalconfirm.go is human-only).
func authorContext(id authorIdentity, teamID uuid.UUID) discussion.AuthorContext {
	agentName, runID := id.agentName, id.runID
	return discussion.AuthorContext{
		Principal: id.principal,
		TeamID:    teamID,
		AgentID:   &agentName,
		RunID:     &runID,
	}
}

// ProposeCreate raises a create_ticket proposal for a work_item_create call made by
// a propose-mode coordinator. The parent_id the auto path requires is irrelevant
// here: the proposal only carries the title/body the human reviews, and the
// confirm fan-out mints the ticket through the board CreateWorkItem seam (which does
// not take an agent-custody parent). An assignee_agent_id on the same call is
// deferred (see proposalToolResult.AssigneeDeferred).
func (p *CoordinatorProposer) ProposeCreate(ctx context.Context, id authorIdentity, a workItemCreateArgs) (any, *jsonrpcError) {
	if a.Title == "" {
		return toolError("work_item_create requires a title")
	}
	thread, deny := p.resolveThread(ctx, id)
	if deny != nil {
		return toolError(*deny)
	}
	payload := discussion.ProposalPayload{
		Action: discussion.ProposalActionCreateTicket,
		Title:  a.Title,
		Body:   a.Body,
	}
	if err := payload.Validate(); err != nil {
		return toolError(err.Error())
	}
	body := "Proposed new ticket: " + a.Title
	msg, err := p.poster.PostProposal(ctx, thread.ProjectID, thread.TeamID, thread.ThreadID,
		authorContext(id, thread.TeamID), body, payload, nil)
	if err != nil {
		return toolError("propose mode: post create_ticket proposal: " + err.Error())
	}
	res := proposalToolResult{
		Proposed:   true,
		Action:     discussion.ProposalActionCreateTicket,
		ProposalID: msg.ID.String(),
		ThreadID:   thread.ThreadID.String(),
		Message:    "Raised a create_ticket proposal for \"" + a.Title + "\"; a human must confirm it before the ticket is created.",
	}
	if a.AssigneeAgentID != "" {
		res.AssigneeDeferred = a.AssigneeAgentID
		res.Message += " The requested assignee was NOT proposed — raise a separate assign_agent proposal once the ticket is confirmed."
	}
	return toolResult(res)
}

// ProposeAssign raises an assign_agent proposal for a work_item_assign call made by
// a propose-mode coordinator. The ticket already exists (the tool takes its id), so
// the proposal carries the ticket id + the requested assignee; the confirm fan-out
// drives the same RequestDispatch the auto path would have.
func (p *CoordinatorProposer) ProposeAssign(ctx context.Context, id authorIdentity, a workItemAssignArgs) (any, *jsonrpcError) {
	if a.ID == "" || a.AssigneeAgentID == "" {
		return toolError("id and assignee_agent_id required")
	}
	thread, deny := p.resolveThread(ctx, id)
	if deny != nil {
		return toolError(*deny)
	}
	payload := discussion.ProposalPayload{
		Action:          discussion.ProposalActionAssignAgent,
		TicketID:        a.ID,
		AssigneeAgentID: a.AssigneeAgentID,
	}
	if err := payload.Validate(); err != nil {
		return toolError(err.Error())
	}
	body := "Proposed assignment: " + a.AssigneeAgentID + " → ticket " + a.ID
	msg, err := p.poster.PostProposal(ctx, thread.ProjectID, thread.TeamID, thread.ThreadID,
		authorContext(id, thread.TeamID), body, payload, nil)
	if err != nil {
		return toolError("propose mode: post assign_agent proposal: " + err.Error())
	}
	return toolResult(proposalToolResult{
		Proposed:   true,
		Action:     discussion.ProposalActionAssignAgent,
		ProposalID: msg.ID.String(),
		ThreadID:   thread.ThreadID.String(),
		Message:    "Raised an assign_agent proposal (" + a.AssigneeAgentID + " → " + a.ID + "); a human must confirm it before the dispatch happens.",
	})
}
