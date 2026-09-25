package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ============================================================================
// Proposal confirm/dismiss shells (ISI-4928, plan ISI-4919 §4.4 story 4 / §6) — POST
// /api/projects/{projectId}/discussion/proposals/{messageId}/confirm and …/dismiss.
// ============================================================================
//
// THE ROOM NEVER EXECUTES. These shells are the ONLY place a proposal becomes an effect, and they
// create nothing new: confirm fans into the EXISTING human authoring seams —
//
//	create_ticket → WorkItemWriter.CreateWorkItem       (the POST /api/projects/{pid}/work-items op)
//	assign_agent  → WorkItemDispatcher.RequestDispatch  (the POST /api/work-items/{id}/dispatch op)
//	party_run     → WorkItemWriter.CreateWorkItem, team set, NO requested_agent (plan §3 T2: the
//	                mint is the story; the intake fan-out that turns it into N Runs is story 3)
//
// — exactly as if the human had clicked the board verbs themselves. Zero new custody semantics:
// custody still moves only in the fenced coord claim tables, and the decision lifecycle (proposed →
// confirmed|dismissed|executed) is a discussion.proposal row, not a coordination record.
//
// RBAC mirrors the authoring wall: HUMAN-ONLY (an agent-authored AuthorContext is 403 — an agent
// may propose, never authorize its own proposal, OQ1/OQ3), mounted behind the §13 BFF authz choke
// point + requireProjectRole(Contributor) in server.go, and sameOrigin-guarded like every other
// state-changing board verb.

// ProposalLifecycle is the discussion-room decision seam these shells drive (implemented by
// *discussion.Store; the interface keeps the room's persistence out of the unit-test lane).
type ProposalLifecycle interface {
	GetProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID) (*discussion.Proposal, error)
	ConfirmProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID, auth discussion.AuthorContext) (*discussion.Proposal, error)
	DismissProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID, auth discussion.AuthorContext) error
	CompleteProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID, auth discussion.AuthorContext, result json.RawMessage, resultBody string) (*discussion.Message, error)
	FailProposal(ctx context.Context, projectID, teamID, messageID uuid.UUID) error
}

// proposalFanout carries the two existing authoring seams (the same interfaces the board's create
// and dispatch handlers ride) plus the optional project-ref resolver, so a confirmed proposal lands
// EXACTLY where the equivalent console click would.
type proposalFanout struct {
	lifecycle ProposalLifecycle
	create    WorkItemWriter
	dispatch  WorkItemDispatcher
	refs      ProjectRefResolver
}

// mapProposalErr maps the lifecycle sentinels onto the HTTP contract (404-not-403 tenancy hiding,
// 409 for an already-decided card).
func mapProposalErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, discussion.ErrProposalNotFound):
		writeJSONError(w, http.StatusNotFound, "proposal not found")
	case errors.Is(err, discussion.ErrProposalNotProposed):
		writeJSONError(w, http.StatusConflict, "proposal already decided")
	default:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

// requireHumanProposalDecider is the OQ1/OQ3 gate: proposing is conversation (any principal), but
// AUTHORIZING a proposal is the human-only authoring wall — identical to work-item create/dispatch.
func requireHumanProposalDecider(w http.ResponseWriter, r *http.Request) (discussion.AuthorContext, bool) {
	auth, ok := discussion.AuthFromContext(r.Context())
	if !ok || auth.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return discussion.AuthorContext{}, false
	}
	if auth.AgentID != nil {
		writeJSONError(w, http.StatusForbidden, "proposal confirmation is human-only; agents may propose, not authorize")
		return discussion.AuthorContext{}, false
	}
	return auth, true
}

// requireProposalPath resolves the shared path vars of both shells: a valid project UUID (the
// room key, same form every discussion route uses) and the proposal message id.
func requireProposalPath(w http.ResponseWriter, r *http.Request) (projectUUID, messageID uuid.UUID, ok bool) {
	projectRef, has := pathVar(r, "projectId")
	if !has || projectRef == "" {
		writeJSONError(w, http.StatusBadRequest, "project id required")
		return uuid.Nil, uuid.Nil, false
	}
	projectUUID, err := uuid.Parse(projectRef)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid projectId")
		return uuid.Nil, uuid.Nil, false
	}
	messageID, err = uuid.Parse(pathVarOr(r, "messageId"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid messageId")
		return uuid.Nil, uuid.Nil, false
	}
	return projectUUID, messageID, true
}

// proposalConfirmHandler answers POST /api/projects/{projectId}/discussion/proposals/{messageId}/confirm.
func proposalConfirmHandler(f proposalFanout) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := requireHumanProposalDecider(w, r)
		if !ok {
			return
		}
		projectUUID, messageID, ok := requireProposalPath(w, r)
		if !ok {
			return
		}

		// 1. Win the one-shot decision lock: proposed → confirmed. The loser of a concurrent
		//    confirm gets 409 and nothing below runs twice. A card that already left `proposed`
		//    is resumed rather than refused — see resumeProposal (ISI-4945: a wedged `confirmed`
		//    card from a post-back failure must still be able to reach `executed`).
		proposal, err := f.lifecycle.ConfirmProposal(r.Context(), projectUUID, auth.TeamID, messageID, auth)
		if errors.Is(err, discussion.ErrProposalNotProposed) {
			f.resumeProposal(w, r, auth, projectUUID, messageID)
			return
		}
		if mapProposalErr(w, err) {
			return
		}

		// 2. Fan into the existing authoring seam for the proposal's action. The room's own team
		//    (proposal.TeamID, from the thread) is the tenancy the mint lands in — the same team
		//    the console create path resolves for this Project.
		f.fanoutAndComplete(w, r, auth, projectUUID, messageID, proposal)
	}
}

// resumeProposal handles a confirm that lost the proposed→confirmed CAS. The card already left
// `proposed`, which is either a real decision or a recovery opportunity (ISI-4945):
//
//   - `executed`  → idempotent success: the earlier Complete committed but its response was lost;
//     re-running the fan-out would duplicate the effect, so answer 200 and stop.
//   - `confirmed` → wedged: the fan-out ran (or was in-flight) but CompleteProposal failed / the
//     process died before the executed transition. Re-run the fan-out and complete. NOTE: the
//     fan-out is not idempotent, so a process crash strictly between the effect and the commit can
//     still double the effect on resume — accepted here (low-severity, narrow window) rather than
//     silently losing the human's confirm intent; full dedup needs an idempotency key on the coord
//     authoring seams, out of scope for this recovery.
//   - `dismissed` → genuinely decided; 409.
func (f proposalFanout) resumeProposal(w http.ResponseWriter, r *http.Request, auth discussion.AuthorContext, projectUUID, messageID uuid.UUID) {
	proposal, err := f.lifecycle.GetProposal(r.Context(), projectUUID, auth.TeamID, messageID)
	if mapProposalErr(w, err) {
		return
	}
	switch proposal.Phase {
	case discussion.ProposalPhaseExecuted:
		writeJSON(w, http.StatusOK, map[string]any{
			"status":          discussion.ProposalPhaseExecuted,
			"proposalId":      messageID.String(),
			"alreadyExecuted": true,
		})
	case discussion.ProposalPhaseConfirmed:
		f.fanoutAndComplete(w, r, auth, projectUUID, messageID, proposal)
	default:
		writeJSONError(w, http.StatusConflict, "proposal already decided")
	}
}

// fanoutAndComplete runs the fan-out for the proposal's action and records the truth: executed +
// post-back on success, roll back to proposed (retryable) on failure. Shared by the fresh-confirm
// and the resume paths so the two stay identical.
func (f proposalFanout) fanoutAndComplete(w http.ResponseWriter, r *http.Request, auth discussion.AuthorContext, projectUUID, messageID uuid.UUID, proposal *discussion.Proposal) {
	result, fanErr := f.execute(r.Context(), auth, projectUUID.String(), proposal)
	if fanErr != nil {
		_ = f.lifecycle.FailProposal(r.Context(), projectUUID, auth.TeamID, messageID)
		if mapWorkItemWriteError(w, fanErr) {
			return
		}
		writeJSONError(w, http.StatusInternalServerError, fanErr.Error())
		return
	}
	body := "Proposal confirmed: " + proposalResultLine(proposal.Payload.Action, result)
	postBack, err := f.lifecycle.CompleteProposal(r.Context(), projectUUID, auth.TeamID, messageID, auth, result, body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "proposal executed but the result post-back failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     discussion.ProposalPhaseExecuted,
		"proposalId": messageID.String(),
		"result":     result,
		"postBack":   postBack,
	})
}

// execute performs the single fan-out for the proposal's action. It writes ONLY through the
// existing authoring seams — it opens no new write path.
func (f proposalFanout) execute(ctx context.Context, auth discussion.AuthorContext, projectRef string, p *discussion.Proposal) (json.RawMessage, error) {
	// Resolve the room's project key exactly like the board create handler does (UIDs pass through).
	projectID, teamUID := projectRef, p.TeamID.String()
	if f.refs != nil {
		resolved, err := f.refs.ResolveProjectRef(ctx, projectRef)
		if err != nil {
			return nil, err
		}
		projectID = resolved.UID
		if resolved.TeamUID != "" {
			teamUID = resolved.TeamUID
		}
	}

	switch p.Payload.Action {
	case discussion.ProposalActionCreateTicket, discussion.ProposalActionPartyRun:
		// party_run's mint is deliberately identical to create_ticket — plan §6: "party items are
		// just items with team_id set and requested_agent NULL". The action label rides in the
		// result payload so the console (story 6) and intake fan-out (story 3) can tell them apart.
		rec, err := f.create.CreateWorkItem(ctx, coord.CreateWorkItemInput{
			ProjectID:         projectID,
			TeamID:            teamUID,
			Title:             p.Payload.Title,
			Body:              p.Payload.Body,
			Principal:         auth.Principal,
			InitiatedByUserID: "",
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{
			"action":     p.Payload.Action,
			"workItemId": rec.ID,
			"state":      rec.State,
			"teamRun":    p.Payload.Action == discussion.ProposalActionPartyRun,
		})

	case discussion.ProposalActionAssignAgent:
		res, err := f.dispatch.RequestDispatch(ctx, coord.RequestDispatchInput{
			WorkItemID: p.Payload.TicketID,
			AgentID:    p.Payload.AssigneeAgentID,
			TeamID:     teamUID,
			Principal:  auth.Principal,
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{
			"action":         p.Payload.Action,
			"workItemId":     res.WorkItemID,
			"fromState":      res.FromState,
			"toState":        res.ToState,
			"requestedAgent": res.RequestedAgent,
		})

	default:
		// Validate() refuses unknown actions at post time; this is the belt-and-suspenders branch.
		return nil, discussion.ErrInvalidProposalPayload
	}
}

// proposalDismissHandler answers POST /api/projects/{projectId}/discussion/proposals/{messageId}/dismiss.
// Dismiss records the decision (proposed → dismissed) and does nothing else — no fan-out, no coord
// write, no lane move.
func proposalDismissHandler(f proposalFanout) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := requireHumanProposalDecider(w, r)
		if !ok {
			return
		}
		projectUUID, messageID, ok := requireProposalPath(w, r)
		if !ok {
			return
		}
		if err := f.lifecycle.DismissProposal(r.Context(), projectUUID, auth.TeamID, messageID, auth); mapProposalErr(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": discussion.ProposalPhaseDismissed})
	}
}

// proposalResultLine is the human-readable post-back body under the proposal card.
func proposalResultLine(action string, result json.RawMessage) string {
	var r struct {
		WorkItemID     string `json:"workItemId"`
		ToState        string `json:"toState"`
		RequestedAgent string `json:"requestedAgent"`
	}
	_ = json.Unmarshal(result, &r)
	switch action {
	case discussion.ProposalActionAssignAgent:
		return "assigned " + r.RequestedAgent + " to work item " + r.WorkItemID + " (" + r.ToState + ")"
	default:
		return "created work item " + r.WorkItemID
	}
}

// pathVarOr is pathVar without the ok-tuple, for call sites that re-parse the value themselves.
func pathVarOr(r *http.Request, key string) string {
	v, _ := pathVar(r, key)
	return v
}
