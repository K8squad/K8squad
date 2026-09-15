package apiserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ============================================================================
// Human work-item COMMENT write (ISI-4406) — POST /api/work-items/{id}/comments,
// the post-box half of the S3 ticket-detail composer (ISI-4399 / design ISI-4231 §3).
// ============================================================================
//
// The ticket thread (GET /api/work-items/{id}) rendered comments read-only because
// the only comment-write path was the agent-only, run-token authed
// POST /api/task-io/post-comment. This handler adds the console/BFF human path behind
// the SAME §13 choke point as the field-edit sibling (workitemwrite.go), so identity
// and Team scope are server-derived — the body is never trusted for authorship.
//
// RBAC mirrors the field-edit endpoint exactly:
//   - Human-only. Agents comment through custody (the run-token path), never the
//     board — an agent-authored AuthorContext is 403.
//   - Keyed only by work-item id (no {projectId} in the path, like the edit/state
//     routes), so it relies on the store's Team scoping: an item outside the caller's
//     Team is 404 (existence-hiding), never a cross-tenant 403.
//
// Author is server-stamped from the session principal (§6.5). On success it returns
// 201 with the persisted comment in the SAME shape the thread read emits
// (coord.TaskComment: {author, body, createdAt}), so the composer can append
// optimistically without a re-fetch.

// WorkItemCommenter is the coord custody surface this endpoint drives — a distinct
// seam from WorkItemWriter so the host can wire (or 501) the comment path
// independently, and tests inject a fake without touching the create/edit fakes. The
// concrete *coord.WorkItemWriteStore satisfies both.
type WorkItemCommenter interface {
	AppendHumanComment(ctx context.Context, workItemID, teamID, principal, body string) (coord.TaskComment, error)
}

// postCommentRequest is the POST body. body is the only field; author is NEVER taken
// from the body — it is server-stamped from the session principal.
type postCommentRequest struct {
	Body string `json:"body"`
}

// workItemCommentHandler answers POST /api/work-items/{id}/comments.
func workItemCommentHandler(store WorkItemCommenter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		// RBAC: human-only. Agents comment via the run-token custody path
		// (/api/task-io/post-comment), never the board.
		if auth.AgentID != nil {
			writeJSONError(w, http.StatusForbidden, "work-item comments are human-only; agents post via custody")
			return
		}
		id, ok := pathVar(r, "id")
		if !ok || id == "" {
			writeJSONError(w, http.StatusBadRequest, "work item id required")
			return
		}

		var req postCommentRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Body == "" {
			writeJSONError(w, http.StatusBadRequest, "comment body required")
			return
		}

		// Team scope mirrors the edit endpoint: the caller's Team fences the item
		// (cross-tenant → 404); a global admin is fleet-unscoped (dangling bootstrap
		// Team, ISI-3921/ISI-4132).
		teamID := authTeamScope(r)

		comment, err := store.AppendHumanComment(r.Context(), id, teamID, auth.Principal, req.Body)
		if mapWorkItemWriteError(w, err) {
			return
		}
		writeJSON(w, http.StatusCreated, comment)
	}
}
