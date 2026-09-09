package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ToolHTTP is the concrete transport for the two untrusted read tools until the shared MCP transport
// (Story 6.2) lands: a thin JSON-over-HTTP surface that maps 1:1 to the ReadService methods. It exists
// so `discussion_search(project)` is actually reachable from the memory service; when 6.2 arrives, the
// same ReadService plugs into the MCP tool registry unchanged (the tool logic is the ReadService, not
// this transport).
//
// S2 HANDOFF (Story J-C / ISI-4010, blocked on Story 6.2): when the shared MCP transport lands, register
// this same ReadService on the real MCP registry (pkg/capability/mcp.go ResolveMCP) with team-from-
// transport scoping (INV3, mirroring the X-Team-Id header discipline below), then retire this surface or
// reduce it to a thin compatibility shim. Do NOT build the transport here — track it against Story 6.2.
//
// The caller's Team scope is taken from the X-Team-Id header — the server-authenticated tenant stamped
// by the §13 BFF, exactly as the discussion apiserver's headerAuth does. It is NEVER read from the
// request body, so a caller cannot widen past its tenant (INV3). project_id/query/top_k are body args.
type ToolHTTP struct {
	read    *ReadService
	write   *WriteService
	discuss DiscussionWriter
}

// DiscussionWriter is the minimal seam the discussion_post tool depends on: the two authored-write
// entry points of the already-fenced discussion store. *discussion.Store satisfies it directly — the
// tool calls the SAME Store methods the REST Handler calls, so provenance-stamping and Team-scope
// enforcement (the fence) are preserved, not duplicated (§7.5 / ADR-0019 §13, ISI-4013). It is nil in
// a read-only deployment, which leaves discussion_post unmounted (AC5).
type DiscussionWriter interface {
	OpenThread(ctx context.Context, projectID uuid.UUID, auth discussion.AuthorContext, title, body string) (*discussion.Thread, error)
	PostMessage(ctx context.Context, projectID, teamID, threadID uuid.UUID, auth discussion.AuthorContext, body string, parentID *uuid.UUID) (*discussion.Message, error)
}

// NewToolHTTP wires the HTTP tool surface to a ReadService and (optionally) a WriteService plus a
// DiscussionWriter. A nil write service leaves memory_write unmounted; a nil discuss writer leaves
// discussion_post unmounted — a read-only deployment exposes only the two search tools, exactly as
// before Stories 6.3 / 6.4.
func NewToolHTTP(read *ReadService, write *WriteService, discuss DiscussionWriter) *ToolHTTP {
	return &ToolHTTP{read: read, write: write, discuss: discuss}
}

// Mount registers the tool endpoints on the given mux.
func (h *ToolHTTP) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/mcp/tools/discussion_search", h.discussionSearch)
	mux.HandleFunc("/mcp/tools/memory_search", h.memorySearch)
	if h.write != nil {
		mux.HandleFunc("/mcp/tools/memory_write", h.memoryWrite)
	}
	if h.discuss != nil {
		mux.HandleFunc("/mcp/tools/discussion_post", h.discussionPost)
	}
}

type searchRequest struct {
	ProjectID string `json:"project_id,omitempty"` // required for discussion_search; ignored by memory_search
	Query     string `json:"query"`
	TopK      int    `json:"top_k,omitempty"`
}

type searchResponse struct {
	// trust is uniformly "untrusted" across results; results carry it per-envelope (§7.3.2).
	Results []Envelope `json:"results"`
}

func (h *ToolHTTP) discussionSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	team := r.Header.Get("X-Team-Id")
	if team == "" {
		http.Error(w, "X-Team-Id required (server-authenticated caller tenant)", http.StatusUnauthorized)
		return
	}
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	out, err := h.read.DiscussionSearch(r.Context(), team, req.ProjectID, req.Query, req.TopK)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, searchResponse{Results: out})
}

func (h *ToolHTTP) memorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	team := r.Header.Get("X-Team-Id")
	if team == "" {
		http.Error(w, "X-Team-Id required (server-authenticated caller tenant)", http.StatusUnauthorized)
		return
	}
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	out, err := h.read.MemorySearch(r.Context(), team, req.Query, req.TopK)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, searchResponse{Results: out})
}

// writeToolRequest is the memory_write body. Scope/authorship are DELIBERATELY absent — team, principal,
// agent, and run are taken from the server-authenticated headers, never the body (WINV1/WINV2). Only
// project_id (a narrowing scope), kind, content, and opaque provenance are body args.
type writeToolRequest struct {
	ProjectID  string          `json:"project_id,omitempty"` // optional narrower scope within the team
	Kind       string          `json:"kind,omitempty"`       // note|fact|diary (default note)
	Content    string          `json:"content"`
	Provenance json.RawMessage `json:"provenance,omitempty"` // opaque caller metadata
}

// writeToolResponse returns the server-assigned id and the stamped scope/kind so the caller can cite
// the record it just wrote (the id is what a §6.4 envelope snapshot pins).
type writeToolResponse struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	TeamID    string `json:"team_id"`
	ProjectID string `json:"project_id,omitempty"`
}

// memoryWrite is the `memory_write` tool: the authorized write path (Story 6.3). Team + principal +
// agent + run are read from the server-stamped headers exactly as the read tools read X-Team-Id — the
// body can neither widen tenancy nor forge authorship. optional() collapses an empty header to nil so
// a human/console-authenticated write (no agent/run) is representable.
func (h *ToolHTTP) memoryWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	team := r.Header.Get("X-Team-Id")
	if team == "" {
		http.Error(w, "X-Team-Id required (server-authenticated caller tenant)", http.StatusUnauthorized)
		return
	}
	principal := r.Header.Get("X-Principal-Id")
	if principal == "" {
		http.Error(w, "X-Principal-Id required (server-authenticated author)", http.StatusUnauthorized)
		return
	}
	var req writeToolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	author := AuthorScope{
		TeamID:    team,
		Principal: principal,
		AgentID:   optional(r.Header.Get("X-Agent-Id")),
		RunID:     optional(r.Header.Get("X-Run-Id")),
	}
	var projectID *string
	if req.ProjectID != "" {
		projectID = &req.ProjectID
	}
	rec, err := h.write.MemoryWrite(r.Context(), author, req.Kind, req.Content, projectID, req.Provenance)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := writeToolResponse{ID: rec.ID, Kind: rec.Kind, TeamID: rec.SquadID}
	if rec.ProjectID != nil {
		resp.ProjectID = *rec.ProjectID
	}
	writeJSON(w, resp)
}

// discussionPostRequest is the discussion_post body. Like memory_write, authorship/tenancy are
// DELIBERATELY absent — Team, principal, agent, and run are taken from the server-authenticated
// headers, never the body (parity with handler AC3). Only the scope (project_id/thread_id/
// parent_message_id) and the content (title/body) are body args, so a forged author_*/created_by/
// team_id has no path to the stored row — the decoder silently drops it.
type discussionPostRequest struct {
	ProjectID       string `json:"project_id"`                  // required: the room to post into (uuid)
	ThreadID        string `json:"thread_id,omitempty"`         // omitted ⇒ open a new thread; present ⇒ reply
	Title           string `json:"title,omitempty"`             // required iff thread_id omitted
	Body            string `json:"body"`                        // thread first-message body, or the reply body
	ParentMessageID string `json:"parent_message_id,omitempty"` // reply target within the thread (ignored when opening)
}

// discussionPost is the `discussion_post` tool: the authored write peer of `discussion_search`. It
// builds a discussion.AuthorContext from the same server-stamped headers memory_write reads and calls
// the SAME fenced Store methods the REST handler calls (OpenThread / PostMessage) — the store, not this
// transport, is where provenance-stamping and Team-scope enforcement live, so the body can neither
// widen tenancy nor forge authorship (WINV1/WINV2, ISI-4013 AC3). thread_id-presence switches
// open-vs-reply. Returns the server-stamped Thread/Message so the agent can cite what it just wrote
// (AC6).
func (h *ToolHTTP) discussionPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	teamHdr := r.Header.Get("X-Team-Id")
	if teamHdr == "" {
		http.Error(w, "X-Team-Id required (server-authenticated caller tenant)", http.StatusUnauthorized)
		return
	}
	principal := r.Header.Get("X-Principal-Id")
	if principal == "" {
		http.Error(w, "X-Principal-Id required (server-authenticated author)", http.StatusUnauthorized)
		return
	}
	teamID, err := uuid.Parse(teamHdr)
	if err != nil {
		http.Error(w, "malformed X-Team-Id (want uuid)", http.StatusBadRequest)
		return
	}
	var req discussionPostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	projectID, err := uuid.Parse(req.ProjectID)
	if err != nil {
		http.Error(w, "malformed project_id (want uuid)", http.StatusBadRequest)
		return
	}
	// Provenance is stamped from `auth` alone — the header identity — mirroring handler AC3. IsAdmin
	// stays false: posting needs no admin and retract is out of scope for the tool.
	auth := discussion.AuthorContext{
		Principal: principal,
		TeamID:    teamID,
		AgentID:   optional(r.Header.Get("X-Agent-Id")),
		RunID:     optional(r.Header.Get("X-Run-Id")),
	}
	// thread_id omitted ⇒ open a new thread (title required); present ⇒ reply to it.
	if req.ThreadID == "" {
		thread, err := h.discuss.OpenThread(r.Context(), projectID, auth, req.Title, req.Body)
		if err != nil {
			writeDiscussionErr(w, err)
			return
		}
		writeJSON(w, thread)
		return
	}
	threadID, err := uuid.Parse(req.ThreadID)
	if err != nil {
		http.Error(w, "malformed thread_id (want uuid)", http.StatusBadRequest)
		return
	}
	var parentID *uuid.UUID
	if req.ParentMessageID != "" {
		pid, err := uuid.Parse(req.ParentMessageID)
		if err != nil {
			http.Error(w, "malformed parent_message_id (want uuid)", http.StatusBadRequest)
			return
		}
		parentID = &pid
	}
	msg, err := h.discuss.PostMessage(r.Context(), projectID, teamID, threadID, auth, req.Body, parentID)
	if err != nil {
		writeDiscussionErr(w, err)
		return
	}
	writeJSON(w, msg)
}

// writeDiscussionErr maps discussion store errors to status codes, mirroring discussion.writeStoreErr:
// tenancy/scope misses are 404-not-403 (a cross-Team/cross-Project thread is indistinguishable from
// absent), empty body/title are 400, everything else 500. Retract/author errors cannot arise here (the
// tool only opens and posts).
func writeDiscussionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, discussion.ErrThreadNotFound), errors.Is(err, discussion.ErrMessageNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, discussion.ErrEmptyBody), errors.Is(err, discussion.ErrEmptyTitle):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// optional collapses an empty header value to nil so an absent optional identity (agent/run) is a nil
// pointer, not an empty-string author column.
func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
