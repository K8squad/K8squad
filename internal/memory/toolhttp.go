package memory

import (
	"encoding/json"
	"net/http"
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
	read  *ReadService
	write *WriteService
}

// NewToolHTTP wires the HTTP tool surface to a ReadService and (optionally) a WriteService. A nil
// write service leaves memory_write unmounted — a read-only deployment exposes only the two search
// tools, exactly as before Story 6.3.
func NewToolHTTP(read *ReadService, write *WriteService) *ToolHTTP {
	return &ToolHTTP{read: read, write: write}
}

// Mount registers the tool endpoints on the given mux.
func (h *ToolHTTP) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/mcp/tools/discussion_search", h.discussionSearch)
	mux.HandleFunc("/mcp/tools/memory_search", h.memorySearch)
	mux.HandleFunc("/mcp/tools/diary_read", h.diaryRead)
	if h.write != nil {
		mux.HandleFunc("/mcp/tools/memory_write", h.memoryWrite)
		mux.HandleFunc("/mcp/tools/diary_append", h.diaryAppend)
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

// diaryReadRequest is the diary_read body. team/agent-default ride the headers (X-Team-Id, X-Agent-Id);
// `agent` optionally overrides whose diary within the team; last_n bounds the count.
type diaryReadRequest struct {
	Agent string `json:"agent,omitempty"`
	LastN int    `json:"last_n,omitempty"`
}

// diaryRead is the `diary_read(agent, last_n)` compat-shim surface (ISI-4077). Team is the X-Team-Id
// server tenant (never the body); `agent` defaults to the caller's own X-Agent-Id when the body omits
// it. Same chronological read + untrusted envelope as the MCP transport.
func (h *ToolHTTP) diaryRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	team := r.Header.Get("X-Team-Id")
	if team == "" {
		http.Error(w, "X-Team-Id required (server-authenticated caller tenant)", http.StatusUnauthorized)
		return
	}
	var req diaryReadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	agent := req.Agent
	if agent == "" {
		agent = r.Header.Get("X-Agent-Id") // default to the caller's own diary, never a widened scope
	}
	if agent == "" {
		http.Error(w, "agent is required (no X-Agent-Id to default to; pass agent)", http.StatusBadRequest)
		return
	}
	out, err := h.read.DiaryRead(r.Context(), team, agent, req.LastN)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, searchResponse{Results: out})
}

// diaryAppendRequest is the diary_append body: just the entry. Kind is fixed to diary and
// authorship/tenancy ride the headers, so there is no kind/project_id/scope body arg by design.
type diaryAppendRequest struct {
	Entry string `json:"entry"`
}

// diaryAppend is the `diary_append(entry)` compat-shim surface (ISI-4077): fixed-kind sugar over the
// authorized write path. Team + principal + agent + run are read from the server-stamped headers
// exactly as memory_write; the body carries only the entry.
func (h *ToolHTTP) diaryAppend(w http.ResponseWriter, r *http.Request) {
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
	var req diaryAppendRequest
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
	rec, err := h.write.DiaryAppend(r.Context(), author, req.Entry)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, writeToolResponse{ID: rec.ID, Kind: rec.Kind, TeamID: rec.SquadID})
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
