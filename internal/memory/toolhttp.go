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
// The caller's Team scope is taken from the X-Team-Id header — the server-authenticated tenant stamped
// by the §13 BFF, exactly as the discussion apiserver's headerAuth does. It is NEVER read from the
// request body, so a caller cannot widen past its tenant (INV3). project_id/query/top_k are body args.
type ToolHTTP struct {
	read *ReadService
}

// NewToolHTTP wires the HTTP tool surface to a ReadService.
func NewToolHTTP(read *ReadService) *ToolHTTP { return &ToolHTTP{read: read} }

// Mount registers the tool endpoints on the given mux.
func (h *ToolHTTP) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/mcp/tools/discussion_search", h.discussionSearch)
	mux.HandleFunc("/mcp/tools/memory_search", h.memorySearch)
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
