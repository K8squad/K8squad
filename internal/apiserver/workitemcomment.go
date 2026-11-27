package apiserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/idemkey"
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
	AppendHumanComment(ctx context.Context, workItemID, teamID, principal, body string, refs []coord.CommentRef) (coord.HumanCommentOutcome, error)
}

// postCommentRequest is the POST body. body is the only required field; author is
// NEVER taken from the body — it is server-stamped from the session principal.
//
// references (ISI-5214, parent ISI-5212 S2) are the structured ticket LINKS the
// ticket-detail `#`-picker collected: work-item UUID + display title, the exact wire
// shape the discussion room's postMessageReq.References carries. They are LINKS, never
// dispatch: coord.AppendHumanComment resolves them against the commented ticket's own
// project and drops any that don't belong (canonicalizing each title from the source),
// so a spoofed label or an out-of-project id never persists. Absent ⇒ a plain comment.
type postCommentRequest struct {
	Body       string             `json:"body"`
	References []coord.CommentRef `json:"references,omitempty"`
}

// workItemCommentHandler answers POST /api/work-items/{id}/comments.
//
// idem (ISI-7907 / ISI-7893 Layer 4) is the process-local short-window replay cache.
// When the caller supplies an Idempotency-Key header, a replay within the window
// returns the ORIGINAL persisted comment and performs NO second insert (and so no
// second ISI-4495 re-dispatch) — the backend half of the duplicate-comment guard
// whose console half is WS-A/WS-B's disabled-during-run composer. An absent header
// behaves exactly as before. A nil cache (DB-less/dev wiring) also degrades to the
// pre-idempotency behaviour.
func workItemCommentHandler(store WorkItemCommenter, idem *idemkey.ReplayCache) http.HandlerFunc {
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

		// ISI-7907: an optional client Idempotency-Key collapses a duplicate POST
		// (double-clicked composer, an at-least-once retry) to ONE insert within the
		// replay window. The key is scoped by (principal, item) under the Team fence so
		// one caller's key can never replay another's comment, and the same literal key
		// on a different ticket stays independent. Over-long keys are a 400 (a
		// bounded-memory guard); an absent key (or an unwired cache) bypasses replay
		// entirely (unchanged behaviour). On a hit we serve the ORIGINAL bytes and run
		// NO store write — and so no second ISI-4495 re-dispatch.
		idemKey := r.Header.Get(idemkey.Header)
		if len(idemKey) > idemkey.MaxKeyLen {
			writeJSONError(w, http.StatusBadRequest, "Idempotency-Key too long")
			return
		}
		scope := ""
		if idemKey != "" && idem != nil {
			scope = idemkey.KeyOf(auth.Principal, "work-item-comment:"+teamID+":"+id, idemKey)
			if status, body, ok := idem.Get(scope); ok {
				idemkey.ServeJSON(w, status, body, true)
				return
			}
		}

		comment, err := store.AppendHumanComment(r.Context(), id, teamID, auth.Principal, req.Body, req.References)
		if mapWorkItemWriteError(w, err) {
			return
		}
		// 201 with the persisted comment (+ the ISI-4495 re-trigger outcome when
		// the comment re-dispatched the item — additive fields the composer uses
		// for its "agent re-triggered" feedback; older readers ignore them). Marshal
		// once so the SAME bytes are both served now and stored for replay.
		body, merr := json.Marshal(comment)
		if merr != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to encode comment")
			return
		}
		if scope != "" {
			idem.Put(scope, http.StatusCreated, body)
		}
		idemkey.ServeJSON(w, http.StatusCreated, body, false)
	}
}
