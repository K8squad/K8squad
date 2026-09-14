package apiserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ============================================================================
// Human work-item DISPATCH (ADR-0022 / ISI-4411) — POST
// /api/work-items/{id}/dispatch, the board's "assign agent → start a Run" verb.
// ============================================================================
//
// The second control-plane POST beside kill (server.go): a single, atomic,
// authorization-bearing action that records the human's agent choice as durable
// intent (coord.work_item.requested_agent) AND advances the lane backlog→todo,
// so the operator Intake sweep mints the Run preferring that agent over the
// hardcoded Team.Spec.Agents[0]. There is NO synchronous poke to the operator —
// "start a Run" IS the lane advance (ADR-0022 §1.2).
//
// A distinct verb (not an agentId bolted onto PATCH .../state) because dispatch
// carries an agent-grant authorization concern foreign to a generic lane move
// (§3 D3): the requested agent MUST belong to the owning Team's composition —
// the §3 D4 check admission (validator.go) does not make. That check is enforced
// in the coord store via the injected TeamAgentResolver, so this shell stays thin.
//
// RBAC mirrors the create/state/edit wall exactly: human-only (an agent-authored
// AuthorContext is 403 — agents progress via custody, never by dispatching the
// board); keyed by item id (no {projectId}), so tenancy is the store's Team
// fence (cross-tenant → 404); Team scope is server-derived (authTeamScope), never
// trusted from the body.

// WorkItemDispatcher is the coord custody op this endpoint drives. The interface
// (not the concrete *coord.WorkItemDispatchStore) is the seam so the host can
// leave the route documented-501 without a DB/cache, and tests can inject a fake.
type WorkItemDispatcher interface {
	RequestDispatch(ctx context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error)
}

// dispatchWorkItemRequest is the POST body: the chosen agent's identifier
// (Team.Spec.Agents[].Name). Nothing else — the Team scope is server-derived, the
// lane advance is implicit in the verb.
type dispatchWorkItemRequest struct {
	AgentID string `json:"agentId"`
}

// workItemDispatchHandler answers POST /api/work-items/{id}/dispatch.
func workItemDispatchHandler(store WorkItemDispatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		// RBAC: human-only. Agents progress through custody, not by dispatching.
		if auth.AgentID != nil {
			writeJSONError(w, http.StatusForbidden, "dispatch is human-only; agents progress via custody")
			return
		}
		id, ok := pathVar(r, "id")
		if !ok || id == "" {
			writeJSONError(w, http.StatusBadRequest, "work item id required")
			return
		}

		var req dispatchWorkItemRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.AgentID == "" {
			writeJSONError(w, http.StatusBadRequest, "agentId required")
			return
		}

		result, err := store.RequestDispatch(r.Context(), coord.RequestDispatchInput{
			WorkItemID:        id,
			AgentID:           req.AgentID,
			TeamID:            authTeamScope(r), // server-derived, never from the body
			Principal:         auth.Principal,
			InitiatedByUserID: "",
		})
		if mapWorkItemWriteError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}
