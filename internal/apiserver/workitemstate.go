package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ============================================================================
// Human board-lane status transition (Story 8.14a / ISI-2909, gap ISI-2876) —
// PATCH /api/work-items/{id}/state, the write side of the §8.6/§13 board.
// ============================================================================
//
// The board is a PROJECTION of coord.work_item.state; the read models existed
// but there was no HTTP write path for a human to move a card between lanes.
// This is the thin auth+mapping shell over coord.HumanStateStore (the custody
// half). It is mounted behind the §13 BFF authz choke point, so identity and
// Team scope are server-derived (AuthorContext) — the body is never trusted for
// identity or tenancy.
//
// RBAC: this is the HUMAN transition endpoint. An agent moves work through
// custody (claim → Complete → handoff, §2.8/§6.1), never by PATCHing a lane, so
// a request whose AuthorContext is agent-authored (AgentID set) is refused 403.
// The transition itself is Team-scoped in the store (cross-tenant → 404).

// WorkItemStateTransitioner is the coord custody op this endpoint drives. The
// interface (not the concrete *coord.HumanStateStore) is the seam so the host
// can leave the route documented-501 without a DB, and tests can inject a fake.
type WorkItemStateTransitioner interface {
	TransitionState(ctx context.Context, workItemID, teamID, targetState, fromState, principal, initiatedByUserID string) (coord.StateTransition, error)
}

// stateTransitionRequest is the PATCH body. toState is the target lane; fromState
// is an optional optimistic-concurrency precondition (the lane the board rendered
// from) — when set, a racing change is rejected 409 instead of silently clobbered.
type stateTransitionRequest struct {
	ToState   string `json:"toState"`
	FromState string `json:"fromState,omitempty"`
}

// workItemStateHandler answers PATCH /api/work-items/{id}/state.
func workItemStateHandler(store WorkItemStateTransitioner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			// Defence in depth: BFFAuthz already guarantees this.
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		// RBAC: human-only. Agents transition via custody, not the board.
		if auth.AgentID != nil {
			writeJSONError(w, http.StatusForbidden, "state transitions are human-only; agents hand off via comment + Complete")
			return
		}

		id, ok := pathVar(r, "id")
		if !ok || id == "" {
			writeJSONError(w, http.StatusBadRequest, "work item id required")
			return
		}

		var req stateTransitionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.ToState == "" {
			writeJSONError(w, http.StatusBadRequest, "toState required")
			return
		}

		result, err := store.TransitionState(r.Context(), id, auth.TeamID.String(), req.ToState, req.FromState, auth.Principal, "")
		switch {
		case errors.Is(err, coord.ErrInvalidState):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, coord.ErrWorkItemNotFound):
			writeJSONError(w, http.StatusNotFound, "work item not found")
			return
		case errors.Is(err, coord.ErrStateConflict):
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		case err != nil:
			writeJSONError(w, http.StatusBadGateway, "state transition unavailable")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// pathVar reads a mux path variable, reporting presence so an unset var (a
// misconfigured route) is a 400, not a silent empty string.
func pathVar(r *http.Request, key string) (string, bool) {
	v, ok := mux.Vars(r)[key]
	return v, ok
}
