package discussion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// ============================================================================
// Auth seam — provenance comes from the authenticated context, NEVER the body
// ============================================================================

type ctxKey struct{}

// WithAuth attaches the caller's server-derived AuthorContext to a request context. In production the
// §13 BFF authz middleware (BFFAuthz below — the same deny-by-default choke point every console read
// model passes) populates this from the session/token/Run; tests inject it directly. The HTTP handlers
// below read the writer's identity from HERE and ignore any author_* in the request body (AC3).
func WithAuth(ctx context.Context, a AuthorContext) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// AuthFromContext returns the AuthorContext placed by the authz middleware, if any.
func AuthFromContext(ctx context.Context) (AuthorContext, bool) {
	a, ok := ctx.Value(ctxKey{}).(AuthorContext)
	return a, ok
}

// Authenticator is the seam the §13 BFF supplies: it turns an inbound request (session cookie, bearer
// token, or Run identity) into the server-derived AuthorContext (principal + Team scope). It is the
// ONLY place a caller's identity/tenancy enters the discussion surface — the handlers never read it
// from the body. `ok=false` ⇒ unauthenticated (the middleware answers 401 and the request never
// reaches a handler, so provenance-stamping fails closed).
type Authenticator interface {
	Authenticate(r *http.Request) (AuthorContext, bool)
}

// BFFAuthz is the §13 authz choke point as mux middleware. Every discussion route is mounted behind it
// (see Mount); a request with no valid AuthorContext is rejected with 401 BEFORE any handler or store
// call runs (deny-by-default). On success the AuthorContext is stamped onto the request context, which
// is the SOLE identity source the write path trusts (AC3).
func BFFAuthz(auth Authenticator) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a, ok := auth.Authenticate(r)
			if !ok || a.Principal == "" {
				writeError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithAuth(r.Context(), a)))
		})
	}
}

// ============================================================================
// Handler
// ============================================================================

// Handler serves the discussion REST surface (§7.5 API). Mounted behind the §13 BFF authz choke point,
// which supplies the AuthorContext (principal + Team scope) — this handler never trusts the body for
// identity or tenancy.
type Handler struct {
	store *Store
}

// NewHandler creates the discussion HTTP handler group.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// Mount wires the discussion surface onto a parent router at the canonical §7.5 prefix
// /api/projects/{projectId}/discussion, behind the §13 BFF authz choke point. This is the one call the
// apiserver makes; `auth` is the BFF's Authenticator. The returned subrouter is the gated group.
func (h *Handler) Mount(parent *mux.Router, auth Authenticator) *mux.Router {
	sub := parent.PathPrefix("/api/projects/{projectId}/discussion").Subrouter()
	sub.Use(BFFAuthz(auth))
	h.Register(sub)
	return sub
}

// Register wires the five §7.5 routes (plus the 10.2 memory-index bridge) onto a subrouter already
// scoped to /api/projects/{projectId}/discussion. Use Mount to also install the authz choke point; a
// bare Register is for tests that inject AuthorContext directly.
func (h *Handler) Register(r *mux.Router) {
	r.HandleFunc("/threads", h.listThreads).Methods(http.MethodGet)                                      // 1. list threads
	r.HandleFunc("/threads", h.openThread).Methods(http.MethodPost)                                      // 2. open thread
	r.HandleFunc("/threads/{threadId}", h.getThread).Methods(http.MethodGet)                             // 3. get thread + messages
	r.HandleFunc("/threads/{threadId}/messages", h.postMessage).Methods(http.MethodPost)                 // 4. post / reply
	r.HandleFunc("/threads/{threadId}/messages/{messageId}", h.retractMessage).Methods(http.MethodPatch) // 5. soft-retract
	// The memory service's incremental-index bridge (10.2 consumer). Same tenancy scope as the reads.
	r.HandleFunc("/memory-index", h.memoryIndex).Methods(http.MethodGet)
}

// ============================================================================
// Helpers
// ============================================================================

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func pathUUID(r *http.Request, key string) (uuid.UUID, bool) {
	id, err := uuid.Parse(mux.Vars(r)[key])
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

func queryInt(r *http.Request, key string, def int) int {
	if s := r.URL.Query().Get(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return def
}

// requireAuth extracts the server-stamped AuthorContext; without it the request is unauthenticated.
// (Under Mount the BFFAuthz middleware already guarantees this — requireAuth is defence-in-depth for a
// handler invoked via a bare Register in tests.)
func requireAuth(w http.ResponseWriter, r *http.Request) (AuthorContext, bool) {
	auth, ok := AuthFromContext(r.Context())
	if !ok || auth.Principal == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return AuthorContext{}, false
	}
	return auth, true
}

// writeStoreErr maps store errors to status codes. Tenancy misses are 404-not-403 (AC5).
func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrThreadNotFound), errors.Is(err, ErrMessageNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrEmptyBody), errors.Is(err, ErrEmptyTitle):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotAuthor):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrAlreadyRetracted):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// ============================================================================
// Thread endpoints
// ============================================================================

func (h *Handler) listThreads(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(r, "projectId")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid projectId")
		return
	}
	auth, ok := requireAuth(w, r)
	if !ok {
		return
	}
	threads, err := h.store.ListThreads(r.Context(), projectID, auth.TeamID,
		queryInt(r, "limit", 50), queryInt(r, "offset", 0))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if threads == nil {
		threads = []Thread{} // an addressable, empty room the instant the Project exists (R1)
	}
	writeJSON(w, http.StatusOK, threads)
}

// openThreadReq deliberately carries NO author_* fields — the writer's identity is server-stamped from
// the authenticated context, so an author supplied in the body is structurally un-representable (AC3).
type openThreadReq struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (h *Handler) openThread(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(r, "projectId")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid projectId")
		return
	}
	auth, ok := requireAuth(w, r)
	if !ok {
		return
	}
	// The request struct carries NO author_* field, so any author/provenance value in the body is
	// silently ignored by the decoder — it has no path to the stored row. Provenance is stamped from
	// `auth` alone, which is what makes impersonation un-representable (AC3).
	var req openThreadReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	thread, err := h.store.OpenThread(r.Context(), projectID, auth, req.Title, req.Body)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, thread)
}

func (h *Handler) getThread(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(r, "projectId")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid projectId")
		return
	}
	threadID, ok := pathUUID(r, "threadId")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid threadId")
		return
	}
	auth, ok := requireAuth(w, r)
	if !ok {
		return
	}
	thread, err := h.store.GetThread(r.Context(), projectID, auth.TeamID, threadID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

// ============================================================================
// Message endpoints
// ============================================================================

// postMessageReq — again, no author_* fields: provenance is server-stamped (AC3).
type postMessageReq struct {
	Body     string  `json:"body"`
	ParentID *string `json:"parentId,omitempty"`
}

func (h *Handler) postMessage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(r, "projectId")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid projectId")
		return
	}
	threadID, ok := pathUUID(r, "threadId")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid threadId")
		return
	}
	auth, ok := requireAuth(w, r)
	if !ok {
		return
	}
	// No author_* field on the struct — a body-supplied author is ignored (AC3); provenance is stamped
	// from `auth`.
	var req postMessageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var parentID *uuid.UUID
	if req.ParentID != nil && *req.ParentID != "" {
		pid, err := uuid.Parse(*req.ParentID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid parentId")
			return
		}
		parentID = &pid
	}
	msg, err := h.store.PostMessage(r.Context(), projectID, auth.TeamID, threadID, auth, req.Body, parentID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

// retractMessage soft-retracts a message (§7.4). Author-or-admin only; there is no hard-delete route.
func (h *Handler) retractMessage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(r, "projectId")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid projectId")
		return
	}
	threadID, ok := pathUUID(r, "threadId")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid threadId")
		return
	}
	messageID, ok := pathUUID(r, "messageId")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid messageId")
		return
	}
	auth, ok := requireAuth(w, r)
	if !ok {
		return
	}
	if err := h.store.Retract(r.Context(), projectID, auth.TeamID, threadID, messageID, auth); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "retracted"})
}

// ============================================================================
// Memory-index bridge (10.2 consumer)
// ============================================================================

func (h *Handler) memoryIndex(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathUUID(r, "projectId")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid projectId")
		return
	}
	auth, ok := requireAuth(w, r)
	if !ok {
		return
	}
	since := time.Unix(0, 0)
	if s := r.URL.Query().Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			since = t
		}
	}
	records, err := h.store.ForMemoryIndex(r.Context(), projectID, auth.TeamID, since, queryInt(r, "limit", 200))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if records == nil {
		records = []MemoryIndexable{}
	}
	writeJSON(w, http.StatusOK, records)
}
