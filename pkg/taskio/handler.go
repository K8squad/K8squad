package taskio

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Store is the coord-backed read/write model the endpoints operate over. It is
// deliberately narrow — the four own-run actions and nothing else. The prod
// implementation binds these onto the coord schema (work_item / comment / claim
// fence); tests bind a fake. The SAME richer read (title/description/AC/goals/
// comments) is shared with S1's assembler `Sources.WorkItem` (ISI-3600) — the
// read model is designed once and lives in pkg/coord, never duplicated.
type Store interface {
	// GetTask returns the work item's full detail scoped to workItemID.
	// ErrNotFound if the item does not exist.
	GetTask(ctx context.Context, workItemID string) (TaskDetail, error)
	// PostComment appends a comment attributed to principal and returns it.
	PostComment(ctx context.Context, workItemID, principal, body string) (Comment, error)
	// UpdateStatus transitions the work item's state. ErrInvalidTransition if
	// the target is not a permitted next state.
	UpdateStatus(ctx context.Context, workItemID, principal, target string) error
	// Checkout claims/refreshes the item via the EXISTING coord.claim fence and
	// returns the fence held after the call. ErrStaleFence if the caller lost a
	// custody race (fence monotonicity preserved).
	Checkout(ctx context.Context, workItemID, principal, runID string) (fence int64, err error)
}

// Sentinel errors a Store may return; the handler maps each to a status code.
var (
	ErrNotFound          = errors.New("taskio: work item not found")
	ErrInvalidTransition = errors.New("taskio: invalid status transition")
	ErrStaleFence        = errors.New("taskio: stale claim fence")
)

// ---- wire shapes (the contract S3's task-io Skill body documents) ----------

// Comment is one provenanced note on a work item.
type Comment struct {
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// TaskDetail is the get-task response: the agent's own task, in full. AC and
// Goals are the richer-read fields shared with S1 (empty until a first-class
// coord surface backs them — see pkg/coord/taskdetail.go).
type TaskDetail struct {
	WorkItemID         string    `json:"workItemId"`
	Title              string    `json:"title"`
	Description        string    `json:"description"`
	State              string    `json:"state"`
	BlockedReason      string    `json:"blockedReason,omitempty"`
	AcceptanceCriteria []string  `json:"acceptanceCriteria,omitempty"`
	Goals              []string  `json:"goals,omitempty"`
	Comments           []Comment `json:"comments"`
	FenceToken         int64     `json:"fenceToken"`
	Holder             string    `json:"holder,omitempty"`
	RunID              string    `json:"runId,omitempty"`
}

// postCommentRequest is the post-comment body: {"body": "..."}.
type postCommentRequest struct {
	Body string `json:"body"`
}

// updateStatusRequest is the update-status body: {"status": "in_review"}.
type updateStatusRequest struct {
	Status string `json:"status"`
}

// checkoutResponse reports the fence held after a successful checkout.
type checkoutResponse struct {
	WorkItemID string `json:"workItemId"`
	RunID      string `json:"runId"`
	FenceToken int64  `json:"fenceToken"`
}

// Handler serves the four own-run endpoints behind run-scoped bearer auth.
type Handler struct {
	minter *Minter
	store  Store
}

// NewHandler binds a verifier and a store. Both are required.
func NewHandler(minter *Minter, store Store) *Handler {
	return &Handler{minter: minter, store: store}
}

// Mux returns the routed, authenticated http.Handler to mount under the coord
// task-io prefix (e.g. /api/task-io). Every route is gated by authn — no
// endpoint is reachable unauthenticated (§AC5).
func (h *Handler) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/get-task", h.auth(h.getTask))
	mux.HandleFunc("/post-comment", h.auth(h.postComment))
	mux.HandleFunc("/update-status", h.auth(h.updateStatus))
	mux.HandleFunc("/checkout", h.auth(h.checkout))
	return mux
}

// authedHandler is a handler that has already resolved the run token.
type authedHandler func(w http.ResponseWriter, r *http.Request, tok RunToken)

// auth is the run-scoped authn middleware: it pulls the bearer token, verifies
// it (fail-closed), and passes the resolved binding down. A missing/expired/
// malformed token is 401; a token scoped to another run is 403. The work item
// and run the inner handler acts on come from the TOKEN, never the request body
// or path — so a client cannot pivot to another run (§AC5).
func (h *Handler) auth(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := bearer(r)
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		tok, err := h.minter.Verify(raw)
		if err != nil {
			// Opaque: bad signature / wrong issuer / expired all read as 401.
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		next(w, r, tok)
	}
}

func (h *Handler) getTask(w http.ResponseWriter, r *http.Request, tok RunToken) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	detail, err := h.store.GetTask(r.Context(), tok.WorkItemID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (h *Handler) postComment(w http.ResponseWriter, r *http.Request, tok RunToken) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req postCommentRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "comment body required")
		return
	}
	// Attribution is the token's principal — never a client-supplied author.
	c, err := h.store.PostComment(r.Context(), tok.WorkItemID, tok.Principal, req.Body)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) updateStatus(w http.ResponseWriter, r *http.Request, tok RunToken) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req updateStatusRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Status == "" {
		writeError(w, http.StatusBadRequest, "status required")
		return
	}
	if err := h.store.UpdateStatus(r.Context(), tok.WorkItemID, tok.Principal, req.Status); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) checkout(w http.ResponseWriter, r *http.Request, tok RunToken) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	fence, err := h.store.Checkout(r.Context(), tok.WorkItemID, tok.Principal, tok.RunID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, checkoutResponse{
		WorkItemID: tok.WorkItemID,
		RunID:      tok.RunID,
		FenceToken: fence,
	})
}

// ---- helpers ---------------------------------------------------------------

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "work item not found")
	case errors.Is(err, ErrInvalidTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid status transition")
	case errors.Is(err, ErrStaleFence):
		writeError(w, http.StatusConflict, "stale claim fence — checkout lost a custody race")
	case errors.Is(err, ErrScopeMismatch):
		writeError(w, http.StatusForbidden, "token not scoped to this work item")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
