package apiserver

// ============================================================================
// Human thread → work-items mint (ISI-7708, parent ISI-7681 P1 board-approved
// plan) — POST /api/projects/{projectId}/discussion/threads/{threadId}/work-items.
// ============================================================================
//
// The discussion room lets a human turn a conversation into board tickets: the
// console "Create tickets from thread" sheet (ISI-7710) posts an array of ticket
// drafts here. This shell creates nothing new — it fans ONLY into the existing
// human authoring seam (WorkItemWriter.CreateWorkItem, the same op POST
// /api/projects/{pid}/work-items rides), exactly like the proposal-confirm
// shell's create_ticket arm. The differences from the proposal path are
// deliberate (plan §5 invariants):
//
//   - HUMAN-INITIATED, not agent-proposed: the drafts come from a human's own
//     selection in the thread (an agent may still PROPOSE tickets via the
//     proposal card — it just cannot author them directly).
//   - PROVENANCE BACKLINK: every created item carries the source thread id and
//     the selected source message ids as labels (ksquad.discussion.thread=<id> /
//     ksquad.discussion.message=<id>), so the board item points back at the
//     conversation that spawned it.
//   - No auto-dispatch in the MVP: items land in the backlog entry lane and
//     nothing is dispatched (picking a lane / assigning stays a board verb).
//
// RBAC mirrors the authoring wall exactly (workitemwrite.go): HUMAN-ONLY (an
// agent-authored AuthorContext is 403), mounted behind the §13 BFF authz choke
// point + requireProjectRole(Contributor) in server.go, sameOrigin-guarded like
// every other state-changing board verb.

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

// DiscussionThreadReader is the thread-existence seam this verb rides (implemented
// by *discussion.Store; the interface keeps the room's persistence out of the
// unit-test lane, same rationale as ProposalLifecycle). GetThread fences on the
// room's team scope, so a thread outside the caller's tenancy is
// ErrThreadNotFound — 404 existence-hiding, indistinguishable from an absent
// thread.
type DiscussionThreadReader interface {
	GetThread(ctx context.Context, projectID string, teamID, threadID uuid.UUID) (*discussion.Thread, error)
}

// Thread-ticket batch + provenance bounds. A batch is bounded so one request
// cannot mint an unbounded row set; the per-draft message-ref cap keeps the
// provenance label set inside coord's maxLabels (20): one thread label plus at
// most 19 message labels.
const (
	maxThreadTickets           = 20
	maxThreadTicketMessageRefs = 19
	threadLabelPrefix          = "ksquad.discussion.thread="
	messageLabelPrefix         = "ksquad.discussion.message="
)

// threadPriorities mirrors coord's create-time priority enum (validPriorities,
// workitemwrite.go) so a bad draft fails as a clean 400 BEFORE any item is
// created — coord re-validates on the write path regardless.
var threadPriorities = map[string]bool{"low": true, "medium": true, "high": true, "urgent": true}

// threadTicketDraft is one human-authored ticket draft. Title is required; body
// and priority are optional pass-throughs (coord validates/normalises). No
// author_* fields (AC3): provenance is stamped from the authenticated context.
// sourceMessageIds optionally cites the thread messages the draft was distilled
// from — the provenance backlink (plan §5).
type threadTicketDraft struct {
	Title            string   `json:"title"`
	Body             string   `json:"body,omitempty"`
	Priority         string   `json:"priority,omitempty"`
	SourceMessageIDs []string `json:"sourceMessageIds,omitempty"`
}

// threadWorkItemsRequest is the POST body: a non-empty batch of drafts.
type threadWorkItemsRequest struct {
	Tickets []threadTicketDraft `json:"tickets"`
}

// threadWorkItemRef is one created item in the response — the board id, the
// entry-lane state it landed in, and the deep link the console card opens (same
// shape as the proposal create_ticket result, ISI-7221 A2).
type threadWorkItemRef struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	State    string   `json:"state"`
	Priority *string  `json:"priority"`
	URL      string   `json:"url"`
	Labels   []string `json:"labels"`
}

// requireHumanThreadTicketer is the human-only gate, identical in shape to
// requireHumanProposalDecider and the work-item create wall: proposing tickets
// is conversation, AUTHORING them is the human-only authoring wall.
func requireHumanThreadTicketer(w http.ResponseWriter, r *http.Request) (discussion.AuthorContext, bool) {
	auth, ok := discussion.AuthFromContext(r.Context())
	if !ok || auth.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return discussion.AuthorContext{}, false
	}
	if auth.AgentID != nil {
		writeJSONError(w, http.StatusForbidden, "thread ticket creation is human-only; agents propose via the proposal card")
		return discussion.AuthorContext{}, false
	}
	return auth, true
}

// threadWorkItemsHandler answers POST
// /api/projects/{projectId}/discussion/threads/{threadId}/work-items: it
// validates the addressed thread exists in the caller's scope, then mints each
// draft through the existing authoring seam with the provenance backlink. See
// the file header for the RBAC wall and the plan §5 invariants.
func threadWorkItemsHandler(threads DiscussionThreadReader, create WorkItemWriter, refs ProjectRefResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := requireHumanThreadTicketer(w, r)
		if !ok {
			return
		}
		projectRef, has := pathVar(r, "projectId")
		if !has || projectRef == "" {
			writeJSONError(w, http.StatusBadRequest, "project id required")
			return
		}
		threadID, err := uuid.Parse(pathVarOr(r, "threadId"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid threadId")
			return
		}

		// Resolve the room's project key exactly like the board create handler
		// does (UIDs pass through): the coord board keys rows by the Project
		// CR UID, and the resolved owning Team is the tenancy the mint lands
		// in — the same team the console create path resolves for this
		// Project (and the same team the room's own scopedAuth fences on,
		// ISI-5198).
		projectID := projectRef
		teamScope := auth.TeamID
		if refs != nil {
			resolved, rerr := refs.ResolveProjectRef(r.Context(), projectRef)
			if mapProjectRefError(w, rerr) {
				return
			}
			projectID = resolved.UID
			if resolved.TeamUID != "" {
				if parsed, perr := uuid.Parse(resolved.TeamUID); perr == nil {
					teamScope = parsed
				}
			}
		}
		if teamScope == uuid.Nil {
			writeJSONError(w, http.StatusBadRequest, "thread ticket creation requires a team scope (ISI-3937)")
			return
		}

		// Thread existence in the caller's scope (404 existence-hiding — a
		// foreign squad's thread id is indistinguishable from an absent one).
		if _, err := threads.GetThread(r.Context(), projectRef, teamScope, threadID); err != nil {
			if errors.Is(err, discussion.ErrThreadNotFound) {
				writeJSONError(w, http.StatusNotFound, "thread not found")
				return
			}
			writeJSONError(w, http.StatusBadGateway, "thread read unavailable")
			return
		}

		var req threadWorkItemsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if len(req.Tickets) == 0 {
			writeJSONError(w, http.StatusBadRequest, "at least one ticket draft required")
			return
		}
		if len(req.Tickets) > maxThreadTickets {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("at most %d ticket drafts per request", maxThreadTickets))
			return
		}
		// Validate EVERY draft before creating ANY item so a bad draft cannot
		// leave a half-minted batch behind (coord re-validates on write).
		for i, t := range req.Tickets {
			if t.Title == "" {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("tickets[%d]: title required", i))
				return
			}
			if t.Priority != "" && !threadPriorities[t.Priority] {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("tickets[%d]: priority %q is not one of low|medium|high|urgent", i, t.Priority))
				return
			}
			if len(t.SourceMessageIDs) > maxThreadTicketMessageRefs {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("tickets[%d]: at most %d sourceMessageIds", i, maxThreadTicketMessageRefs))
				return
			}
			for _, mid := range t.SourceMessageIDs {
				if _, err := uuid.Parse(mid); err != nil {
					writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("tickets[%d]: invalid sourceMessageId %q", i, mid))
					return
				}
			}
		}

		// Mint each draft through the EXISTING authoring seam. Items land in
		// the backlog entry lane with NO state set at create (structural:
		// coord.CreateWorkItemInput carries no State field) and carry the
		// provenance backlink labels. The loop is sequential best-effort, not
		// a batch transaction: an infrastructure failure mid-batch reports 500
		// with the items that DID land so the caller can retry the remainder.
		created := make([]threadWorkItemRef, 0, len(req.Tickets))
		for i, t := range req.Tickets {
			labels := make([]string, 0, 1+len(t.SourceMessageIDs))
			labels = append(labels, threadLabelPrefix+threadID.String())
			for _, mid := range t.SourceMessageIDs {
				labels = append(labels, messageLabelPrefix+mid)
			}
			rec, err := create.CreateWorkItem(r.Context(), coord.CreateWorkItemInput{
				ProjectID:         projectID,
				TeamID:            teamScope.String(),
				Title:             t.Title,
				Body:              t.Body,
				Priority:          t.Priority,
				Labels:            labels,
				Principal:         auth.Principal,
				InitiatedByUserID: auth.UserID,
			})
			if err != nil {
				if len(created) == 0 {
					mapWorkItemWriteError(w, err)
					return
				}
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error":   fmt.Sprintf("ticket %d of %d failed: %v", i+1, len(req.Tickets), err),
					"created": created,
				})
				return
			}
			recLabels := rec.Labels
			if recLabels == nil {
				recLabels = labels
			}
			created = append(created, threadWorkItemRef{
				ID:       rec.ID,
				Title:    rec.Title,
				State:    rec.State,
				Priority: rec.Priority,
				URL:      workItemURL(projectRef, rec.ID),
				Labels:   recLabels,
			})
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"status":   "created",
			"threadId": threadID.String(),
			"count":    len(created),
			"created":  created,
		})
	}
}
