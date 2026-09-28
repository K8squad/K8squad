package discussion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/pkg/search"
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
	store    *Store
	searcher search.Searcher
	org      OrgReader
}

// NewHandler creates the discussion HTTP handler group.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// NewHandlerWithDeps creates the discussion HTTP handler with additional dependencies for mention
// search (ISI-4926): `searcher` is the work-item FTS read model (pkg/search — the same searcher
// /api/search rides), `org` is the Team roster seam below. Either may be nil (DB-less dev runs):
// the mentions route then degrades to whatever sources are wired, mirroring the documented-501
// discipline of the sibling read models without unmounting the discussion group.
func NewHandlerWithDeps(store *Store, searcher search.Searcher, org OrgReader) *Handler {
	return &Handler{store: store, searcher: searcher, org: org}
}

// TeamAgent is one agent on the caller's Team as the mention composer renders it: the mention
// token (@Name) plus the derived presence bucket, so the popover can badge it (§5 roster).
type TeamAgent struct {
	Name   string
	Status string
}

// OrgReader is the roster seam the §13 BFF supplies for mention + roster resolution (ISI-4926,
// ISI-5107): it lists agents, scoped one of three ways so the caller can be shown "the agents
// dispatchable into THIS project" rather than the agents of the caller's own Team. Production adapts
// the apiserver org projection (cmd/apiserver); tests wire a fake. It is deliberately blind to work
// items — those come from the search.Searcher seam — so each source keeps its own fence.
type OrgReader interface {
	// TeamAgents lists the agents of ONE Team (the caller's own authorized scope) — the non-admin
	// fence: existence-hiding is preserved because a non-admin never resolves any other Team.
	TeamAgents(ctx context.Context, teamID uuid.UUID) ([]TeamAgent, error)
	// ProjectAgents lists the agents of the project's OWN team, derived from the project id's
	// "namespace/name" slug (the namespace IS the team home namespace, ISI-5107). Used for admins,
	// who carry no home team to fence to — the project namespace IS the dispatch scope.
	ProjectAgents(ctx context.Context, projectID string) ([]TeamAgent, error)
	// AllAgents lists agents fleet-wide, across all teams (ADR-039). Used for the admin @-mention
	// composer so a global admin can pull an agent from ANY squad into the room.
	AllAgents(ctx context.Context) ([]TeamAgent, error)
}

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
	// ISI-4928 (plan §4.4): the inert action-proposal card. Any authenticated principal may
	// propose; the confirm/dismiss shells (apiserver) are what gate execution.
	r.HandleFunc("/threads/{threadId}/proposals", h.postProposal).Methods(http.MethodPost)
	// ISI-4930 (plan §4.7 story 6 read side): the thread's proposal cards + lifecycle phase.
	// The message read stays phase-less; this join is the durable card state after a reload.
	r.HandleFunc("/threads/{threadId}/proposals", h.listProposals).Methods(http.MethodGet)
	// The memory service's incremental-index bridge (10.2 consumer). Same tenancy scope as the reads.
	r.HandleFunc("/memory-index", h.memoryIndex).Methods(http.MethodGet)
	// Mention search endpoint (ISI-4926): agent + ticket suggestions for the @-mention composer.
	r.HandleFunc("/mentions", h.searchMentions).Methods(http.MethodGet)
	// Project roster endpoint (ISI-5107): the right-rail agent list, scoped to the agents
	// dispatchable into THIS project (not the caller's team). Resolved at read time from the path.
	r.HandleFunc("/roster", h.roster).Methods(http.MethodGet)
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

// pathProjectID extracts the {projectId} path variable as the platform Project id — a Kubernetes
// "namespace/name" composite (ISI-3982), NOT a uuid. The root router runs with UseEncodedPath(), so
// the captured value carries one layer of percent-encoding ("namespace%2Fname"); we unescape it once
// to recover the canonical "namespace/name" the store keys rooms by — the same reversal the apiserver
// applies uniformly via decodePathVar. An empty id is rejected (ok=false → 400) so a missing/blank
// segment cannot silently scope to the whole squad.
func pathProjectID(r *http.Request) (string, bool) {
	raw := mux.Vars(r)["projectId"]
	id, err := url.PathUnescape(raw)
	if err != nil {
		id = raw // a malformed escape degrades to the raw value rather than dropping the var
	}
	if id == "" {
		return "", false
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
	case errors.Is(err, ErrThreadNotFound), errors.Is(err, ErrMessageNotFound),
		errors.Is(err, ErrProposalNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrEmptyBody), errors.Is(err, ErrEmptyTitle),
		errors.Is(err, ErrInvalidAudience), errors.Is(err, ErrInvalidKind),
		errors.Is(err, ErrInvalidProposalPayload):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotAuthor):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrAlreadyRetracted), errors.Is(err, ErrProposalNotProposed):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// ============================================================================
// Thread endpoints
// ============================================================================

func (h *Handler) listThreads(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProjectID(r)
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
	projectID, ok := pathProjectID(r)
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
	projectID, ok := pathProjectID(r)
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

// postMessageReq — no author_* fields: provenance is server-stamped (AC3). audience, kind, and
// payload are optional wire fields; the store applies 'party'/'text' defaults and validation.
type postMessageReq struct {
	Body     string           `json:"body"`
	ParentID *string          `json:"parentId,omitempty"`
	Audience *string          `json:"audience,omitempty"`
	Kind     *string          `json:"kind,omitempty"`
	Payload  *json.RawMessage `json:"payload,omitempty"`
}

func (h *Handler) postMessage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProjectID(r)
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
	msg, err := h.store.PostMessage(r.Context(), projectID, auth.TeamID, threadID, auth, req.Body, parentID, req.Audience, req.Kind, req.Payload)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

// retractMessage soft-retracts a message (§7.4). Author-or-admin only; there is no hard-delete route.
func (h *Handler) retractMessage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProjectID(r)
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
// Proposal endpoint (ISI-4928, plan §4.4)
// ============================================================================

// postProposalReq — no author_* fields (AC3, same as every write here); the structured payload is
// the action contract from plan §6. Provenance of the PROPOSER is server-stamped; an agent may
// propose (that is the point — propose, never execute).
type postProposalReq struct {
	Body    string          `json:"body"`
	Payload ProposalPayload `json:"payload"`
}

// postProposal appends an inert kind='proposal' message (phase='proposed'). It writes no coord row
// and moves no custody — the fan-out happens only in the human-gated confirm shell.
func (h *Handler) postProposal(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProjectID(r)
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
	var req postProposalReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	msg, err := h.store.PostProposal(r.Context(), projectID, auth.TeamID, threadID, auth, req.Body, req.Payload, nil)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

// listProposals answers GET /threads/{threadId}/proposals: every proposal card in the thread with
// its lifecycle phase (story 6 read side). Tenancy misses are indistinguishable from an empty
// thread of another team — a foreign thread id yields 404 via the store's scope probe.
func (h *Handler) listProposals(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProjectID(r)
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
	proposals, err := h.store.ListProposals(r.Context(), projectID, auth.TeamID, threadID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proposals)
}

// ============================================================================
// Memory-index bridge (10.2 consumer)
// ============================================================================

func (h *Handler) memoryIndex(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProjectID(r)
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
	records, err := h.store.ForMemoryIndex(r.Context(), projectID, auth.TeamID, since, queryInt(r, "limit", 200), auth.Principal, auth.AgentID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if records == nil {
		records = []MemoryIndexable{}
	}
	writeJSON(w, http.StatusOK, records)
}

// MentionSuggestion is one result from the mention search — either an agent or a work item.
type MentionSuggestion struct {
	Type        string  `json:"type"`        // "agent" or "work_item"
	ID          string  `json:"id"`          // Agent name (the @-mention token) or work item UUID
	DisplayName string  `json:"displayName"` // Agent name or work item title
	ProjectID   string  `json:"projectId"`   // Owning project UUID (for work items only)
	State       string  `json:"state"`       // Work item lane, or the agent's presence bucket
	Rank        float64 `json:"rank"`        // Relevance rank (work items only; 0 for agents)
}

// MentionSearchResponse is the GET /api/projects/{projectId}/discussion/mentions payload. Results
// is always a JSON array (never null) so the composer can render an empty state without a nil guard.
type MentionSearchResponse struct {
	Query   string              `json:"query"`
	Results []MentionSuggestion `json:"results"`
}

// Composer-sized caps: a mention popover is a short list, not a search page.
const (
	mentionWorkItemLimit = 10
	mentionAgentLimit    = 5
)

// searchMentions answers GET /api/projects/{projectId}/discussion/mentions?q=… with agent + work
// item suggestions scoped to the project/team (ISI-4926, plan §4.3). It rides the same §13 BFF
// authz choke point as every discussion route; the tenancy scope is derived from the
// server-stamped AuthorContext, never from the request. Agents come first (a mention popover
// leads with people); work items follow, fenced by the ADR-039 in-query predicate and then
// narrowed to the path's project.
func (h *Handler) searchMentions(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid projectId")
		return
	}
	auth, ok := requireAuth(w, r)
	if !ok {
		return
	}

	text := r.URL.Query().Get("q")
	if text == "" {
		writeError(w, http.StatusBadRequest, "q (search text) required")
		return
	}

	results := make([]MentionSuggestion, 0)

	// Agents first, from the Team roster seam. A roster error degrades to ticket-only
	// suggestions — the composer stays usable; the roster is a projection, not the fence.
	if h.org != nil {
		agentResults, err := h.searchAgents(r.Context(), text, auth)
		if err == nil {
			results = append(results, agentResults...)
		}
	}

	// Work items via the global-search read model (pkg/search): the RBAC scope rides the query
	// (AllTeams ONLY for admins, TeamID otherwise — exactly the /api/search contract), then the
	// handler narrows to the path's project. A searcher failure is the search plane failing —
	// surface it (502) rather than silently answering with agents only.
	if h.searcher != nil {
		searchResults, err := h.searchWorkItems(r.Context(), text, projectID, auth)
		switch {
		case errors.Is(err, search.ErrEmptyQuery):
			// A query of only stopwords/punctuation parses to an empty tsquery — same contract
			// as /api/search (400, not 500).
			writeError(w, http.StatusBadRequest, "q (search text) required")
			return
		case err != nil:
			writeError(w, http.StatusBadGateway, "mention search unavailable")
			return
		default:
			results = append(results, searchResults...)
		}
	}

	writeJSON(w, http.StatusOK, MentionSearchResponse{Query: text, Results: results})
}

// searchWorkItems searches the work-item corpus via the global search service, scoped to the
// caller's Team (or fleet-wide for admins — ADR-039) and then narrowed to the path's project.
func (h *Handler) searchWorkItems(ctx context.Context, text, projectID string, auth AuthorContext) ([]MentionSuggestion, error) {
	q := search.Query{
		Text:     text,
		Limit:    mentionWorkItemLimit,
		AllTeams: auth.IsAdmin,         // admin: fleet-wide (ADR-039)…
		TeamID:   auth.TeamID.String(), // …everyone else: fenced to their Team (§12.1)
	}

	searchResults, err := h.searcher.Search(ctx, q)
	if err != nil {
		return nil, err
	}

	suggestions := make([]MentionSuggestion, 0, len(searchResults))
	for _, result := range searchResults {
		// The mention composer suggests tickets of THIS project; rows that predate project
		// scoping (ProjectID "") are dropped rather than guessed at.
		if projectID != "" && result.ProjectID != projectID {
			continue
		}
		suggestions = append(suggestions, MentionSuggestion{
			Type:        "work_item",
			ID:          result.ID,
			DisplayName: result.Title,
			ProjectID:   result.ProjectID,
			State:       result.State,
			Rank:        result.Rank,
		})
	}
	return suggestions, nil
}

// searchAgents resolves @agent suggestions: a case-insensitive substring match on the agent name —
// mention typing is fragment matching, not FTS. The scope mirrors the ADR-039 branch searchWorkItems
// already uses: an admin (who carries no home team to fence to) resolves fleet-wide so they can pull
// an agent from ANY squad into the room; everyone else is fenced to their own Team, so no cross-team
// name can appear (existence-hiding preserved, ISI-5107 RBAC note).
func (h *Handler) searchAgents(ctx context.Context, text string, auth AuthorContext) ([]MentionSuggestion, error) {
	var (
		agents []TeamAgent
		err    error
	)
	if auth.IsAdmin {
		agents, err = h.org.AllAgents(ctx)
	} else {
		agents, err = h.org.TeamAgents(ctx, auth.TeamID)
	}
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(text)
	out := make([]MentionSuggestion, 0, mentionAgentLimit)
	for _, a := range agents {
		if a.Name == "" || !strings.Contains(strings.ToLower(a.Name), needle) {
			continue
		}
		out = append(out, MentionSuggestion{
			Type:        "agent",
			ID:          a.Name, // the @-mention token is the agent name
			DisplayName: a.Name,
			State:       a.Status,
		})
		if len(out) >= mentionAgentLimit {
			break
		}
	}
	return out, nil
}

// ============================================================================
// Roster endpoint (ISI-5107)
// ============================================================================

// RosterAgent is one row of the project roster read: the agent's mention token (@Name) and derived
// presence bucket. The `id` mirrors the name so the console can key rows and direct-target the agent
// with the same token the composer inserts — a roster is namespace-scoped, so names are unique.
type RosterAgent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// roster answers GET /api/projects/{projectId}/discussion/roster with the agents dispatchable into
// THIS project (ISI-5107) — the fix for the empty right rail. Scope mirrors the ADR-039 branch: an
// admin resolves the roster from the PROJECT's namespace (the slug's namespace IS the team home
// namespace), since the bootstrap admin carries no home team to fence to; everyone else is fenced to
// their own Team (existence-hiding — a non-admin never sees another squad's agents). Resolved at read
// time from the path's project, so no thread.team_id backfill is required. The roster is a projection,
// not a fence, so any read failure degrades to an empty rail (200) rather than blanking the room.
func (h *Handler) roster(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid projectId")
		return
	}
	auth, ok := requireAuth(w, r)
	if !ok {
		return
	}
	out := make([]RosterAgent, 0)
	if h.org != nil {
		agents, err := h.projectRoster(r.Context(), projectID, auth)
		if err == nil {
			for _, a := range agents {
				if a.Name == "" {
					continue
				}
				out = append(out, RosterAgent{ID: a.Name, Name: a.Name, Status: a.Status})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// projectRoster resolves the right-rail roster for a project: admin ⇒ the project namespace's agents
// (ADR-039 fleet break-out, narrowed to the squad the room belongs to); non-admin ⇒ the caller's own
// Team (existence-hiding preserved — a non-admin cannot name another squad's namespace to read it).
func (h *Handler) projectRoster(ctx context.Context, projectID string, auth AuthorContext) ([]TeamAgent, error) {
	if auth.IsAdmin {
		return h.org.ProjectAgents(ctx, projectID)
	}
	return h.org.TeamAgents(ctx, auth.TeamID)
}
