package taskio

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/telemetry"
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
	// UpdateStatus transitions the work item's state and returns the lane it was
	// in BEFORE the move (for the AC8 `status.from` span attribute). from may be
	// empty when the prior lane is unknown (e.g. an early validation refusal).
	// ErrInvalidTransition if the target is not a permitted next state.
	UpdateStatus(ctx context.Context, workItemID, principal, target string) (from string, err error)
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
	// auth resolves the run token FIRST (so an unauthenticated caller never opens
	// a span — AC8's GIVEN is "invoked with a valid run token"); instrument then
	// opens the taskio.<op> server span with the token's run/work-item identity.
	mux.HandleFunc("/get-task", h.auth(h.instrument("get_task", h.getTask)))
	mux.HandleFunc("/post-comment", h.auth(h.instrument("post_comment", h.postComment)))
	mux.HandleFunc("/update-status", h.auth(h.instrument("update_status", h.updateStatus)))
	mux.HandleFunc("/checkout", h.auth(h.instrument("checkout", h.checkout)))
	return mux
}

// instrument opens the AC8 `taskio.<op>` server span around a handler. It joins
// the Run's distributed trace: the client (S3's skill runtime) forwards the
// Run's `traceparent` — stamped into the pod env by dispatch — as an inbound
// header, which is Extracted here so each call is a child of the Run trace.
// Attributes are op/ids/status only; comment bodies and task descriptions are
// NEVER emitted (§8 PII rule). A metric hook can attach at span end after
// ISI-3593 without re-plumbing.
func (h *Handler) instrument(op string, next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, tok RunToken) {
		ctx := telemetry.Extract(r.Context(), inboundTrace(r))
		ctx, span := telemetry.Tracer().Start(ctx, "taskio."+op,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("ksquad.taskio.op", op),
				attribute.String("ksquad.run.id", tok.RunID),
				attribute.String("ksquad.taskio.work_item_id", tok.WorkItemID),
			),
		)
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next(rec, r.WithContext(ctx), tok)

		span.SetAttributes(attribute.Int("http.response.status_code", rec.code))
		if rec.code >= http.StatusBadRequest {
			span.SetAttributes(attribute.String("ksquad.taskio.error_class", errorClass(rec.code)))
			span.SetStatus(codes.Error, http.StatusText(rec.code))
		}
	}
}

// inboundTrace lifts the W3C trace-context headers a task-io client forwards
// into a carrier for telemetry.Extract. Absent headers root a fresh trace.
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

// statusRecorder captures the response status code for the AC8 span without
// buffering the body. Defaults to 200 (a handler that Writes without an explicit
// WriteHeader).
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

// errorClass maps a >=400 status to a low-cardinality span attribute value (§8:
// bounded enum, never free text). 401 never reaches here — auth short-circuits
// before the span opens.
func errorClass(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "stale_fence"
	case http.StatusUnprocessableEntity:
		return "invalid_transition"
	default:
		if code >= http.StatusInternalServerError {
			return "server_error"
		}
		return "client_error"
	}
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
	from, err := h.store.UpdateStatus(r.Context(), tok.WorkItemID, tok.Principal, req.Status)
	// AC8: status.from/.to are recorded ONLY for update_status. .to is the
	// requested target (always known); .from is the prior lane the store
	// reports (may be empty on a refusal where the current lane is not read).
	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(attribute.String("ksquad.taskio.status.to", req.Status))
	if from != "" {
		span.SetAttributes(attribute.String("ksquad.taskio.status.from", from))
	}
	if err != nil {
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
