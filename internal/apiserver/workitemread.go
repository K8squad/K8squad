package apiserver

// workitemread.go — M1.5 (ISI-4131): the board READ surface, the thin auth+mapping
// shell over coord.WorkItemReadStore (the custody half). Two endpoints:
//
//   - GET /api/projects/{projectId}/work-items — the per-Project board card
//     list (§13 projection of coord.work_item.state, plus the live claim holder
//     and thread sizes). This is the list the console Issues tab (M1.6) draws
//     from; the POST on the same path is the S3 create (ISI-3959).
//   - GET /api/work-items/{id} — the full ticket thread: agent-authored
//     comments, the recent status-change history (§6.5 audit), and the
//     agent-reported change refs (commits/PR links). The M1.5 AC ("ticket shows
//     agent-authored comments + status changes + change refs without human
//     relay") is answered by THIS payload.
//
// RBAC mirrors the sibling board routes (workitemwrite.go):
//   - Reads are open to any authenticated principal behind the §13 choke point
//     (human OR agent — visibility is not authoring); the list route adds
//     requireProjectRole(Viewer) when a membership resolver is wired.
//   - Tenancy is server-derived (AuthorContext), never from the body: the store
//     fences by Team — an item outside the caller's Team is 404
//     (existence-hiding), never a cross-tenant 403.

import (
	"context"
	"errors"
	"net/http"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// WorkItemReader is the coord board-read surface these endpoints drive. The
// interface (not the concrete *coord.WorkItemReadStore) is the seam so the host
// can leave the routes documented-501 without a DB, and tests can inject a fake.
type WorkItemReader interface {
	// ListWorkItems returns the board cards for one Project, Team-scoped.
	ListWorkItems(ctx context.Context, teamID, projectID string) ([]coord.BoardItem, error)
	// ReadWorkItemThread returns one ticket's full thread (comments, status
	// history, change refs), Team-scoped.
	ReadWorkItemThread(ctx context.Context, workItemID, teamID string) (coord.WorkItemThread, error)
}

// workItemListHandler answers GET /api/projects/{projectId}/work-items.
func workItemListHandler(store WorkItemReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := discussion.AuthFromContext(r.Context()); !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		projectID, ok := pathVar(r, "projectId")
		if !ok || projectID == "" {
			writeJSONError(w, http.StatusBadRequest, "project id required")
			return
		}
		teamID := authTeamScope(r)
		items, err := store.ListWorkItems(r.Context(), teamID, projectID)
		if mapWorkItemReadError(w, err) {
			return
		}
		// Always a JSON array, never null, so the console renders an empty board.
		if items == nil {
			items = []coord.BoardItem{}
		}
		writeJSON(w, http.StatusOK, items)
	}
}

// workItemThreadHandler answers GET /api/work-items/{id}.
func workItemThreadHandler(store WorkItemReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := discussion.AuthFromContext(r.Context()); !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		id, ok := pathVar(r, "id")
		if !ok || id == "" {
			writeJSONError(w, http.StatusBadRequest, "work item id required")
			return
		}
		teamID := authTeamScope(r)
		thread, err := store.ReadWorkItemThread(r.Context(), id, teamID)
		if mapWorkItemReadError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, thread)
	}
}

// authTeamScope derives the caller's Team fence from the server-stamped
// AuthorContext. The zero Team UUID is the fleet-admin "no bound Team" marker —
// passed as "" (the store's trusted unscoped path).
func authTeamScope(r *http.Request) string {
	auth, ok := discussion.AuthFromContext(r.Context())
	if !ok {
		return ""
	}
	if auth.TeamID.String() == "00000000-0000-0000-0000-000000000000" {
		return ""
	}
	return auth.TeamID.String()
}

// mapWorkItemReadError maps the store's sentinels the same way the write shell
// does (workitemwrite.go): 404 existence-hiding, 502 for infrastructure.
// Returns true when it wrote an error response (the caller should stop).
func mapWorkItemReadError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, coord.ErrWorkItemNotFound):
		writeJSONError(w, http.StatusNotFound, "work item not found")
	default:
		writeJSONError(w, http.StatusBadGateway, "work-item read unavailable")
	}
	return true
}
