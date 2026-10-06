package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ============================================================================
// decision_request answer/reject shells (ISI-5536, ADR-0026 §4.4/§5) — POST
// /api/projects/{projectId}/discussion/decision-requests/{messageId}/answer and …/reject.
// ============================================================================
//
// A decision_request is the net-new "an agent suggests structured options a human answers" surface.
// Asking is conversation (an agent OR a human may post the card — that create lives thread-scoped in
// internal/discussion/handler.go, like postProposal). ANSWERING is the human-only decision, and these
// shells are the only place a decision becomes forward progress. They copy proposalconfirm.go
// wholesale and introduce ZERO new custody semantics:
//
//   - The answer/reject itself is a CAS on the discussion.decision_request lifecycle row + a
//     kind='structured' post-back into the thread (the audit trail the agent reads on its next run) —
//     both committed atomically in the store (AnswerDecisionRequest/RejectDecisionRequest).
//   - CONTINUATION (ADR-0026 §5) reuses the EXISTING dispatch seam: there is no suspended process to
//     resume — a k8squad run is stateless-per-dispatch — so "resume_agent_on_answer" is concretely a
//     WorkItemDispatcher.RequestDispatch of the card's work item (the identical seam assign_agent
//     proposals fan out through). RequestDispatch performs the lane move itself (its ISI-4808 re-run
//     branch advances a parked in_review ticket back into the working lane) and records the agent, so
//     the re-dispatched run reads the human's answer from the thread. Custody still moves only in the
//     fenced coord claim tables.
//
// RBAC mirrors the proposal wall: HUMAN-ONLY answer/reject (an agent-authored AuthorContext is 403 —
// an agent may ask, never answer its own ask), mounted behind the BFF authz choke point +
// requireProjectRole(Contributor) in server.go, sameOrigin-guarded like every other board verb.

// DecisionLifecycle is the discussion-room decision seam these shells drive (implemented by
// *discussion.Store; the interface keeps the room's persistence out of the unit-test lane).
type DecisionLifecycle interface {
	GetDecisionRequest(ctx context.Context, projectID string, teamID, messageID uuid.UUID) (*discussion.DecisionRequest, error)
	AnswerDecisionRequest(ctx context.Context, projectID string, teamID, messageID uuid.UUID, auth discussion.AuthorContext, answer discussion.DecisionAnswer) (*discussion.Message, error)
	RejectDecisionRequest(ctx context.Context, projectID string, teamID, messageID uuid.UUID, auth discussion.AuthorContext, reason string) (*discussion.Message, error)
}

// decisionFanout carries the lifecycle seam plus the SINGLE existing authoring seam the continuation
// reuses (the same WorkItemDispatcher the board dispatch + proposal assign_agent ride).
type decisionFanout struct {
	lifecycle DecisionLifecycle
	dispatch  WorkItemDispatcher
}

// mapDecisionErr maps the lifecycle sentinels onto the HTTP contract (404-not-403 tenancy hiding,
// 409 for an already-decided card, 400 for a bad payload / a missing required reject reason).
func mapDecisionErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, discussion.ErrDecisionRequestNotFound):
		writeJSONError(w, http.StatusNotFound, "decision_request not found")
	case errors.Is(err, discussion.ErrDecisionRequestNotOpen):
		writeJSONError(w, http.StatusConflict, "decision_request already decided")
	case errors.Is(err, discussion.ErrRejectReasonRequired):
		writeJSONError(w, http.StatusBadRequest, "this decision_request requires a reason to reject")
	case errors.Is(err, discussion.ErrInvalidDecisionPayload):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

// requireHumanDecisionDecider is the human-only gate: asking is conversation (any principal), but
// ANSWERING/REJECTING is the human-only authoring wall — identical to proposal confirm/dismiss.
func requireHumanDecisionDecider(w http.ResponseWriter, r *http.Request) (discussion.AuthorContext, bool) {
	auth, ok := discussion.AuthFromContext(r.Context())
	if !ok || auth.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return discussion.AuthorContext{}, false
	}
	if auth.AgentID != nil {
		writeJSONError(w, http.StatusForbidden, "answering a decision_request is human-only; agents may ask, not answer")
		return discussion.AuthorContext{}, false
	}
	return auth, true
}

// decisionAnswerReq is the human's typed answer (ADR-0026 §4.3 wire-in). The mode is read from the
// stored card, never the body — the client supplies only the selection and/or the free-text.
type decisionAnswerReq struct {
	SelectedOptionIDs []string `json:"selectedOptionIds,omitempty"`
	FreeText          *string  `json:"freeText,omitempty"`
}

// decisionRejectReq carries only the reason; rejectRequiresReason is enforced against the stored card.
type decisionRejectReq struct {
	Reason string `json:"reason,omitempty"`
}

// decisionAnswerHandler answers POST /api/projects/{projectId}/discussion/decision-requests/{messageId}/answer.
func decisionAnswerHandler(f decisionFanout) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := requireHumanDecisionDecider(w, r)
		if !ok {
			return
		}
		projectID, messageID, ok := requireProposalPath(w, r) // shared {projectId}+{messageId} parse
		if !ok {
			return
		}
		var req decisionAnswerReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		// Read the card first so the answer can be validated against its mode/options before the CAS.
		dr, err := f.lifecycle.GetDecisionRequest(r.Context(), projectID, auth.TeamID, messageID)
		if mapDecisionErr(w, err) {
			return
		}
		answer, verr := buildDecisionAnswer(dr.Payload, req.SelectedOptionIDs, req.FreeText)
		if verr != nil {
			mapDecisionErr(w, verr)
			return
		}

		// Win the one-shot decision: open → answered, persisting the typed answer + the post-back in
		// one tx. The loser of a concurrent answer gets 409.
		postBack, err := f.lifecycle.AnswerDecisionRequest(r.Context(), projectID, auth.TeamID, messageID, auth, answer)
		if mapDecisionErr(w, err) {
			return
		}

		// Continuation (ADR-0026 §5): the answer is already durable in the thread, so resuming the
		// raising agent is BEST-EFFORT — a dispatch failure must not fail the human's decision.
		resumed, resumeErr := f.resumeRaisingAgent(r.Context(), auth, dr)
		writeDecisionResult(w, discussion.DecisionPhaseAnswered, messageID, postBack, resumed, resumeErr)
	}
}

// decisionRejectHandler answers POST /api/projects/{projectId}/discussion/decision-requests/{messageId}/reject.
func decisionRejectHandler(f decisionFanout) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := requireHumanDecisionDecider(w, r)
		if !ok {
			return
		}
		projectID, messageID, ok := requireProposalPath(w, r)
		if !ok {
			return
		}
		var req decisionRejectReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		dr, err := f.lifecycle.GetDecisionRequest(r.Context(), projectID, auth.TeamID, messageID)
		if mapDecisionErr(w, err) {
			return
		}
		if !dr.Payload.AllowReject {
			writeJSONError(w, http.StatusBadRequest, "this decision_request does not allow rejection")
			return
		}
		if dr.Payload.RejectRequiresReason && req.Reason == "" {
			mapDecisionErr(w, discussion.ErrRejectReasonRequired)
			return
		}

		postBack, err := f.lifecycle.RejectDecisionRequest(r.Context(), projectID, auth.TeamID, messageID, auth, req.Reason)
		if mapDecisionErr(w, err) {
			return
		}

		// A rejection is still a human decision the agent must act on — resume it so it reads the
		// rejection + reason from the thread and adjusts (same best-effort continuation as answer).
		resumed, resumeErr := f.resumeRaisingAgent(r.Context(), auth, dr)
		writeDecisionResult(w, discussion.DecisionPhaseRejected, messageID, postBack, resumed, resumeErr)
	}
}

// resumeRaisingAgent re-dispatches the card's work item so the raising agent reads the human's
// decision from the thread on its next run (ADR-0026 §5 / D4). It reuses WorkItemDispatcher.
// RequestDispatch verbatim — the lane move (in_review → working, via the ISI-4808 re-run branch) and
// the requested-agent record happen inside that proven seam; this shell adds no new custody path.
//
// Returns (dispatched, err). It is a no-op (false, nil) when the card binds no work item or was raised
// by a human (no agent to resume) — the ADR's "dispatch the ticket assignee when the raiser differs"
// refinement needs a coord assignee read and is deferred; v1 resumes the raising agent, which is the
// agent that asked and is therefore the one holding the question.
func (f decisionFanout) resumeRaisingAgent(ctx context.Context, auth discussion.AuthorContext, dr *discussion.DecisionRequest) (bool, error) {
	if f.dispatch == nil || dr.WorkItemID == "" {
		return false, nil
	}
	if dr.Message.AuthorAgentID == nil || *dr.Message.AuthorAgentID == "" {
		return false, nil // human-raised card: no agent run to resume (assignee-read refinement deferred)
	}
	_, err := f.dispatch.RequestDispatch(ctx, coord.RequestDispatchInput{
		WorkItemID: dr.WorkItemID,
		AgentID:    *dr.Message.AuthorAgentID,
		TeamID:     dr.TeamID.String(),
		Principal:  auth.Principal,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// writeDecisionResult renders the uniform 200 for answer/reject: the new phase, the card id, the
// thread post-back, and whether (and how) the raising agent was resumed. A resume error is surfaced
// as a non-fatal note — the decision itself succeeded.
func writeDecisionResult(w http.ResponseWriter, phase string, messageID uuid.UUID, postBack *discussion.Message, resumed bool, resumeErr error) {
	body := map[string]any{
		"status":            phase,
		"decisionRequestId": messageID.String(),
		"postBack":          postBack,
		"agentResumed":      resumed,
	}
	if resumeErr != nil {
		body["resumeError"] = resumeErr.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

// buildDecisionAnswer validates the human's selection against the card's mode/options (ADR-0026 §4.1)
// and assembles the typed DecisionAnswer. Validation errors wrap ErrInvalidDecisionPayload (→400).
func buildDecisionAnswer(p discussion.DecisionRequestPayload, selected []string, freeText *string) (discussion.DecisionAnswer, error) {
	ans := discussion.DecisionAnswer{Mode: p.Mode, SelectedOptionIDs: selected, FreeText: freeText}
	hasFreeText := freeText != nil && *freeText != ""

	optionIDs := make(map[string]struct{}, len(p.Options))
	for _, o := range p.Options {
		optionIDs[o.ID] = struct{}{}
	}
	validSelection := func() error {
		seen := make(map[string]struct{}, len(selected))
		for _, id := range selected {
			if _, ok := optionIDs[id]; !ok {
				return invalidDecision("selected option %q is not offered by this decision_request", id)
			}
			if _, dup := seen[id]; dup {
				return invalidDecision("option %q selected twice", id)
			}
			seen[id] = struct{}{}
		}
		return nil
	}

	switch p.Mode {
	case discussion.DecisionModeApprove:
		// Approve is an acceptance; it carries no option selection. (Rejection takes the /reject path.)
		ans.SelectedOptionIDs = nil

	case discussion.DecisionModeFreeForm:
		if !hasFreeText {
			return ans, invalidDecision("mode free_form requires a free-text answer")
		}
		ans.SelectedOptionIDs = nil

	case discussion.DecisionModeChooseOne:
		if len(selected) == 0 {
			if p.AllowFreeText && hasFreeText {
				break // the "something else" path
			}
			return ans, invalidDecision("mode choose_one requires exactly one selected option")
		}
		if len(selected) != 1 {
			return ans, invalidDecision("mode choose_one requires exactly one selected option, got %d", len(selected))
		}
		if err := validSelection(); err != nil {
			return ans, err
		}

	case discussion.DecisionModeChooseMany:
		if len(selected) == 0 {
			if p.AllowFreeText && hasFreeText {
				break
			}
			return ans, invalidDecision("mode choose_many requires at least one selected option")
		}
		if err := validSelection(); err != nil {
			return ans, err
		}
		minSel := p.MinSelected
		if minSel <= 0 {
			minSel = 1
		}
		if len(selected) < minSel {
			return ans, invalidDecision("mode choose_many requires at least %d selected, got %d", minSel, len(selected))
		}
		if p.MaxSelected > 0 && len(selected) > p.MaxSelected {
			return ans, invalidDecision("mode choose_many permits at most %d selected, got %d", p.MaxSelected, len(selected))
		}

	default:
		return ans, invalidDecision("unknown decision mode %q", p.Mode)
	}

	// free_form's free-text IS the answer (allowFreeText is implied); for every other mode free-text
	// is the opt-in "something else" channel and is refused unless the card enables it.
	if p.Mode != discussion.DecisionModeFreeForm && !p.AllowFreeText && hasFreeText {
		return ans, invalidDecision("this decision_request does not allow a free-text answer")
	}
	return ans, nil
}

// invalidDecision wraps ErrInvalidDecisionPayload so buildDecisionAnswer's failures map to 400
// (errors.Is(…, ErrInvalidDecisionPayload) holds, and mapDecisionErr renders the specific message).
func invalidDecision(format string, args ...any) error {
	return fmt.Errorf("%w: %s", discussion.ErrInvalidDecisionPayload, fmt.Sprintf(format, args...))
}
