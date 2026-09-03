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
	mux.HandleFunc("/create-agent", h.auth(h.instrument("create_agent", h.requireScope(ScopeOrgWrite, h.createAgent))))
	mux.HandleFunc("/create-skill", h.auth(h.instrument("create_skill", h.requireScope(ScopeOrgWrite, h.createSkill))))
	mux.HandleFunc("/create-project", h.auth(h.instrument("create_project", h.requireScope(ScopeProjectWrite, h.createProject))))
	mux.HandleFunc("/archive-project", h.auth(h.instrument("archive_project", h.requireScope(ScopeProjectWrite, h.archiveProject))))
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
