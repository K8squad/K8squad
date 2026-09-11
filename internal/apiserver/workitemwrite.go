package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ============================================================================
// Human work-item CREATE + FIELD-EDIT (Story S3 / ISI-3959) — POST
// /api/projects/{projectId}/work-items and PATCH /api/work-items/{id}, the
// write siblings of the §8.6/§13 board's state-transition (workitemstate.go).
// ============================================================================
//
// The board (a projection of coord.work_item, §13) had a MOVE path but no way to
// CREATE an item or EDIT its fields. These two handlers are the thin auth+mapping
// shell over coord.WorkItemWriteStore (the custody half), mounted behind the §13
// BFF authz choke point so identity and Team scope are server-derived
// (AuthorContext) — the body is never trusted for identity or tenancy.
//
// RBAC mirrors the state endpoint's authority model exactly:
//   - Human-only. Agents move work through custody (claim → Complete → handoff),
//     never by authoring board items, so an agent-authored AuthorContext is 403.
//   - Create is project-scoped, so the route also carries requireProjectRole(
//     Contributor) (server.go): a viewer/non-member is refused at the wall.
//   - Edit is keyed only by work-item id (no {projectId} in the path, exactly like
//     the state route), so it relies on the store's Team scoping: an item outside
//     the caller's Team is 404 (existence-hiding), never a cross-tenant 403.
//
// STATE IS NOT A FIELD. Create lands in the default entry lane; edit never carries
// state (that stays on the .../state path) so the board-derivation invariant holds.

// WorkItemWriter is the coord custody surface these endpoints drive. The interface
// (not the concrete *coord.WorkItemWriteStore) is the seam so the host can leave the
// routes documented-501 without a DB, and tests can inject a fake.
type WorkItemWriter interface {
	CreateWorkItem(ctx context.Context, in coord.CreateWorkItemInput) (coord.WorkItemRecord, error)
	UpdateWorkItem(ctx context.Context, workItemID string, in coord.UpdateWorkItemInput) (coord.WorkItemRecord, error)
}

// createWorkItemRequest is the POST body. title is required; parentId makes it a
// sub-issue. State is intentionally absent — a new item lands in the default entry
// lane; picking a lane is a board move (ISI-2909), not a create.
type createWorkItemRequest struct {
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	ParentID string `json:"parentId,omitempty"`
}

// updateWorkItemRequest is the PATCH body. Each field is a pointer so an absent field
// is "leave unchanged" and a present-but-empty field is a real clear. State is absent
// by design. expectedUpdatedAt is the optimistic-concurrency precondition.
type updateWorkItemRequest struct {
	Title             *string `json:"title,omitempty"`
	Body              *string `json:"body,omitempty"`
	ParentID          *string `json:"parentId,omitempty"`
	ExpectedUpdatedAt string  `json:"expectedUpdatedAt,omitempty"`
}

// workItemCreateHandler answers POST /api/projects/{projectId}/work-items.
// refs (nil-tolerant) translates the console's "namespace/name" id to the
// Project CR UID and pins root items to the Project's OWNING Team (ISI-4132) —
// the board and the intake dispatcher agree on that tenancy, so a ticket created
// from the console is immediately dispatchable.
func workItemCreateHandler(store WorkItemWriter, refs ProjectRefResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		// RBAC: human-only. Agents author work through custody, not the board.
		if auth.AgentID != nil {
			writeJSONError(w, http.StatusForbidden, "work-item authoring is human-only; agents create via custody")
			return
		}
		projectID, ok := pathVar(r, "projectId")
		if !ok || projectID == "" {
			writeJSONError(w, http.StatusBadRequest, "project id required")
			return
		}
		// Resolve the console's project ref to the CR identity. The Project's
		// owning Team is the root item's tenancy — uniform for members and
		// fleet-admins alike (a member's Team IS the namespace owner), so the
		// dangling-team admin path (ISI-3921) stops being a special case here.
		projectTeam := ""
		if refs != nil {
			resolved, err := refs.ResolveProjectRef(r.Context(), projectID)
			if mapProjectRefError(w, err) {
				return
			}
			projectID = resolved.UID
			projectTeam = resolved.TeamUID
		}

		var req createWorkItemRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Title == "" {
			writeJSONError(w, http.StatusBadRequest, "title required")
			return
		}

		// Team scope: the resolved Project's owning Team wins (above); without a
		// resolver the caller's Team becomes the item's team as before, and for a
		// sub-issue the store overrides with the parent's (§6.1). A fleet-admin
		// with no bound Team creating a ROOT item against an unresolved project
		// ref keeps the honest refusal until the ISI-3937 selector lands.
		teamID := projectTeam
		if teamID == "" && auth.TeamID.String() != "00000000-0000-0000-0000-000000000000" {
			teamID = auth.TeamID.String()
		}
		if teamID == "" && req.ParentID == "" {
			writeJSONError(w, http.StatusBadRequest, "fleet-admin root-issue create requires team selection (ISI-3937)")
			return
		}

		rec, err := store.CreateWorkItem(r.Context(), coord.CreateWorkItemInput{
			ProjectID:         projectID,
			TeamID:            teamID,
			ParentID:          req.ParentID,
			Title:             req.Title,
			Body:              req.Body,
			Principal:         auth.Principal,
			InitiatedByUserID: "",
		})
		if mapWorkItemWriteError(w, err) {
			return
		}
		writeJSON(w, http.StatusCreated, rec)
	}
}

// workItemEditHandler answers PATCH /api/work-items/{id}.
func workItemEditHandler(store WorkItemWriter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		if auth.AgentID != nil {
			writeJSONError(w, http.StatusForbidden, "work-item edits are human-only; agents progress via custody")
			return
		}
		id, ok := pathVar(r, "id")
		if !ok || id == "" {
			writeJSONError(w, http.StatusBadRequest, "work item id required")
			return
		}

		var req updateWorkItemRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Title == nil && req.Body == nil && req.ParentID == nil {
			writeJSONError(w, http.StatusBadRequest, "at least one editable field required")
			return
		}

		// Team scope mirrors the state endpoint: the caller's Team fences the item
		// (cross-tenant → 404). An admin still passes its Team here; fleet-wide
		// cross-team edit is the ISI-3937 selector's job (not per-user rebind).
		teamID := auth.TeamID.String()
		if teamID == "00000000-0000-0000-0000-000000000000" {
			teamID = ""
		}

		rec, err := store.UpdateWorkItem(r.Context(), id, coord.UpdateWorkItemInput{
			Title:             req.Title,
			Body:              req.Body,
			ParentID:          req.ParentID,
			ExpectedUpdatedAt: req.ExpectedUpdatedAt,
			TeamID:            teamID,
			Principal:         auth.Principal,
			InitiatedByUserID: "",
		})
		if mapWorkItemWriteError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, rec)
	}
}

// mapWorkItemWriteError maps the store's sentinel errors to status codes the SAME
// way the state handler does, so the whole board write surface answers one shape.
// Returns true when it wrote an error response (the caller should stop).
func mapWorkItemWriteError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, coord.ErrInvalidWorkItem):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, coord.ErrWorkItemNotFound):
		writeJSONError(w, http.StatusNotFound, "work item not found")
	case errors.Is(err, coord.ErrStateConflict):
		writeJSONError(w, http.StatusConflict, err.Error())
	default:
		writeJSONError(w, http.StatusBadGateway, "work-item write unavailable")
	}
	return true
}
