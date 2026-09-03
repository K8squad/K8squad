package orgops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/taskio"
	"github.com/K8squad/K8squad/pkg/telemetry"
)

// Store is the coord-backed write model the org/project verbs operate over. It
// is deliberately narrow — the four privileged actions and nothing else. The
// prod implementation (store.go) binds these onto the cluster via a
// controller-runtime client, resolving the target namespace from the calling
// Run so a create can only land in the run's OWN squad namespace; tests bind a
// fake. Every method takes the verified RunToken so the store can scope the
// write to the run and stamp provenance — the handler has already enforced the
// token's scope before any Store method is reached.
type Store interface {
	CreateAgent(ctx context.Context, tok taskio.RunToken, req AgentInput) (Result, error)
	CreateSkill(ctx context.Context, tok taskio.RunToken, req SkillInput) (Result, error)
	CreateProject(ctx context.Context, tok taskio.RunToken, req ProjectInput) (Result, error)
	ArchiveProject(ctx context.Context, tok taskio.RunToken, name string) (Result, error)
	// Assign resolves the A2A handoff carrier for handing work to a teammate
	// (there is no coord assignee row — I4/no-P2P). It validates the target
	// agent exists in the calling run's namespace and returns the carrier.
	Assign(ctx context.Context, tok taskio.RunToken, req AssignInput) (AssignResult, error)
}

// Sentinel errors a Store may return; the handler maps each to a status code.
var (
	// ErrValidation is a field-level rejection (bad name, missing required
	// field, or admission/CEL/webhook refusal) → 422.
	ErrValidation = errors.New("orgops: validation failed")
	// ErrConflict is a strict-create collision (the object already exists) → 409.
	ErrConflict = errors.New("orgops: object already exists")
	// ErrNotFound is an archive/target miss (no such project) → 404.
	ErrNotFound = errors.New("orgops: object not found")
	// ErrNamespaceUnresolved means the run token's RunID resolved to no Run (or a
	// Run with no namespace), so there is nowhere to write → 404 (existence-
	// hiding: a caller with no resolvable run has no org to mutate).
	ErrNamespaceUnresolved = errors.New("orgops: run namespace unresolved")
)

// ---- wire shapes -----------------------------------------------------------

type objectRefInput struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type secretRefInput struct {
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}

// AgentInput is the create-agent body. It mirrors the console compose agent
// shape MINUS the `project` membership field — org-ops does not gate on console
// membership, it gates on the run token's org:write scope, and the namespace is
// the calling run's own, not a client-named project.
type AgentInput struct {
	Name                string           `json:"name"`
	RuntimeRef          objectRefInput   `json:"runtimeRef"`
	RoleRef             objectRefInput   `json:"roleRef"`
	Model               string           `json:"model"`
	CredentialSecretRef secretRefInput   `json:"credentialSecretRef"`
	SkillRefs           []objectRefInput `json:"skillRefs,omitempty"`
	ModelEndpointRef    *secretRefInput  `json:"modelEndpointRef,omitempty"`
}

// SkillInput is the create-skill body (register a Skill CR).
type SkillInput struct {
	Name   string `json:"name"`
	Source struct {
		Type   string `json:"type"`
		Inline string `json:"inline,omitempty"`
		Git    *struct {
			RepoRef string `json:"repoRef"`
			Ref     string `json:"ref"`
			Path    string `json:"path,omitempty"`
		} `json:"git,omitempty"`
	} `json:"source"`
	Permissions []string `json:"permissions,omitempty"`
}

// ProjectInput is the create-project body.
type ProjectInput struct {
	Name string `json:"name"`
	Repo struct {
		URL string `json:"url"`
		Ref string `json:"ref,omitempty"`
	} `json:"repo"`
	Goals []string `json:"goals,omitempty"`
}

// archiveProjectRequest is the archive-project body: {"name": "..."}.
type archiveProjectRequest struct {
	Name string `json:"name"`
}

// AssignInput is the assign-work body: hand a work item to a teammate agent.
// The coord model has NO assignee column and NO agent-to-agent row by design
// (I4/no-P2P, migrations 0001/0004), so "assign to a teammate" is not a board-
// row write — its sanctioned carrier is an A2A SubmitTask to the target agent's
// card (ADR-0005 rev.2 D4). This endpoint authorizes the intent (org:write) and
// resolves the A2A carrier; the actual SubmitTask is performed by the caller's
// runtime A2A client. To ENQUEUE claimable work instead, use task-io /subtask.
type AssignInput struct {
	ToAgent    string `json:"toAgent"`
	WorkItemID string `json:"workItemId"`
	Summary    string `json:"summary,omitempty"`
}

// A2ACarrier is the resolved agent-to-agent handoff descriptor the assign verb
// returns: everything the runtime's existing A2A client (pkg/a2a V1 SubmitTask,
// V6 GetAgentCard) needs to hand work to the teammate, addressed by the same
// Agent Card the platform already advertises.
type A2ACarrier struct {
	Verb         string `json:"verb"`         // "SubmitTask" (A2A V1)
	TargetAgent  string `json:"targetAgent"`  // resolved Agent metadata.name
	TargetSquad  string `json:"targetSquad"`  // the Agent's namespace (squad)
	AgentCardRef string `json:"agentCardRef"` // "<namespace>/<name>" — the runtime resolves this to the shim card
}

// AssignResult reports the resolved carriers for an assign intent. A2A is the
// direct handoff carrier; CoordEnqueue names the alternative (enqueue claimable
// work) so a skill can ship both and let the runtime pick.
type AssignResult struct {
	WorkItemID   string     `json:"workItemId,omitempty"`
	ToAgent      string     `json:"toAgent"`
	A2A          A2ACarrier `json:"a2a"`
	CoordEnqueue string     `json:"coordEnqueue"` // "/api/task-io/subtask"
}

// Result is the response body for a successful verb: the kind/name/namespace the
// write landed in plus the operation performed.
type Result struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Operation string `json:"operation"` // "created" | "archived"
}

// Handler serves the four privileged endpoints behind run-scoped bearer auth
// with per-verb scope enforcement.
type Handler struct {
	minter *taskio.Minter
	store  Store
}

// NewHandler binds a verifier and a store. Both are required.
func NewHandler(minter *taskio.Minter, store Store) *Handler {
	return &Handler{minter: minter, store: store}
}

// Mux returns the routed, authenticated http.Handler to mount under the org-ops
// prefix (e.g. /api/org-ops). Every route is gated by authn AND by the verb's
// required scope — no privileged verb is reachable without both a valid run
// token and the role-derived scope that token carries.
func (h *Handler) Mux() http.Handler {
	mux := http.NewServeMux()
	// Canonical noun routes (ADR-0005 rev.2 D4): these are the paths the org-ops
	// skill body's HTTP tool definitions target, so the tool is callable
	// end-to-end.
	agents := h.auth(h.instrument("create_agent", h.requireScope(ScopeOrgWrite, h.createAgent)))
	skills := h.auth(h.instrument("create_skill", h.requireScope(ScopeOrgWrite, h.createSkill)))
	projects := h.auth(h.instrument("create_project", h.requireScope(ScopeProjectWrite, h.createProject)))
	mux.HandleFunc("/agents", agents)
	mux.HandleFunc("/skills", skills)
	mux.HandleFunc("/projects", projects)
	mux.HandleFunc("/assign", h.auth(h.instrument("assign", h.requireScope(ScopeOrgWrite, h.assign))))
	mux.HandleFunc("/archive-project", h.auth(h.instrument("archive_project", h.requireScope(ScopeProjectWrite, h.archiveProject))))
	// Verb aliases (the original ISI-3626 shapes) kept callable so an early
	// caller minted against them does not break; the nouns above are canonical.
	mux.HandleFunc("/create-agent", agents)
	mux.HandleFunc("/create-skill", skills)
	mux.HandleFunc("/create-project", projects)
	return mux
}

// authedHandler is a handler that has already resolved the run token.
type authedHandler func(w http.ResponseWriter, r *http.Request, tok taskio.RunToken)

// auth pulls and verifies the bearer run token (fail-closed): a missing/expired/
// malformed token is 401. The token is the SAME KSQUAD_COORD_TOKEN the task-io
// seam mints — verified via the shared taskio.Minter — so one run credential
// drives both surfaces.
func (h *Handler) auth(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := bearer(r)
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		tok, err := h.minter.Verify(raw)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		next(w, r, tok)
	}
}

// requireScope is the AUTHORIZATION gate (ADR-0005 D2). A token that verifies
// but lacks `want` is 403 — authentic, but its role does not grant this verb.
// This is where an IC (or a self-added org-ops skill on a leaf role) is denied:
// its token carries no such scope regardless of the skill body it holds.
func (h *Handler) requireScope(want string, next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, tok taskio.RunToken) {
		if !tok.HasScope(want) {
			// Record the denial on the span (bounded enum, no free text) so the
			// o11y path can count out-of-scope attempts (ISI-3592).
			span := trace.SpanFromContext(r.Context())
			span.SetAttributes(attribute.String("ksquad.orgops.required_scope", want))
			writeError(w, http.StatusForbidden, "run token lacks required scope: "+want)
			return
		}
		next(w, r, tok)
	}
}

func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request, tok taskio.RunToken) {
	if !post(w, r) {
		return
	}
	var req AgentInput
	if !decode(w, r, &req) {
		return
	}
	res, err := h.store.CreateAgent(r.Context(), tok, req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (h *Handler) createSkill(w http.ResponseWriter, r *http.Request, tok taskio.RunToken) {
	if !post(w, r) {
		return
	}
	var req SkillInput
	if !decode(w, r, &req) {
		return
	}
	res, err := h.store.CreateSkill(r.Context(), tok, req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (h *Handler) createProject(w http.ResponseWriter, r *http.Request, tok taskio.RunToken) {
	if !post(w, r) {
		return
	}
	var req ProjectInput
	if !decode(w, r, &req) {
		return
	}
	res, err := h.store.CreateProject(r.Context(), tok, req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (h *Handler) archiveProject(w http.ResponseWriter, r *http.Request, tok taskio.RunToken) {
	if !post(w, r) {
		return
	}
	var req archiveProjectRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "project name required")
		return
	}
	res, err := h.store.ArchiveProject(r.Context(), tok, req.Name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) assign(w http.ResponseWriter, r *http.Request, tok taskio.RunToken) {
	if !post(w, r) {
		return
	}
	var req AssignInput
	if !decode(w, r, &req) {
		return
	}
	if req.ToAgent == "" {
		writeError(w, http.StatusBadRequest, "toAgent required")
		return
	}
	res, err := h.store.Assign(r.Context(), tok, req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- instrumentation (orgops.<op> span, parity with taskio AC8) ------------

// instrument opens an `orgops.<op>` server span joined to the Run trace via the
// inbound traceparent the run's org-ops client forwards. Attributes are
// op/ids/scope/status only — NEVER agent bodies, skill contents, or project
// goals (§8 PII rule).
func (h *Handler) instrument(op string, next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, tok taskio.RunToken) {
		ctx := telemetry.Extract(r.Context(), inboundTrace(r))
		ctx, span := telemetry.Tracer().Start(ctx, "orgops."+op,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("ksquad.orgops.op", op),
				attribute.String("ksquad.run.id", tok.RunID),
			),
		)
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next(rec, r.WithContext(ctx), tok)

		span.SetAttributes(attribute.Int("http.response.status_code", rec.code))
		if rec.code >= http.StatusBadRequest {
			span.SetAttributes(attribute.String("ksquad.orgops.error_class", errorClass(rec.code)))
			span.SetStatus(codes.Error, http.StatusText(rec.code))
		}
	}
}

func inboundTrace(r *http.Request) map[string]string {
	c := map[string]string{}
	if v := r.Header.Get("traceparent"); v != "" {
		c["traceparent"] = v
	}
	if v := r.Header.Get("tracestate"); v != "" {
		c["tracestate"] = v
	}
	return c
}

type statusRecorder struct {
	http.ResponseWriter
	code    int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.code = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

func errorClass(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusForbidden:
		return "out_of_scope"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusUnprocessableEntity:
		return "validation"
	default:
		if code >= http.StatusInternalServerError {
			return "server_error"
		}
		return "client_error"
	}
}

// ---- helpers ---------------------------------------------------------------

func post(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return false
	}
	return true
}

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
	case errors.Is(err, ErrValidation):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "object already exists in this namespace")
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "object not found")
	case errors.Is(err, ErrNamespaceUnresolved):
		writeError(w, http.StatusNotFound, "no namespace for this run")
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
