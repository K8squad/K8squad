package taskio

import (
	"context"
	"errors"
	"net/http"
)

// decisionraise.go — the run-scoped request-decision verb (ISI-7224,
// ISI-7074 WS-6.2): the emission path EVERY runtime can raise a board decision
// through. The client supplies ONLY the ask; attribution (principal/run) and
// targeting (work item) come from the run token's binding, never the body —
// the same §AC5 discipline as every other verb here.
//
// The seam is deliberately separate from Store: the decision card lives in the
// discussion schema (thread-scoped, ADR-0026), not coord, so the raise binds
// to a DecisionRaiser wired by the host (cmd/apiserver adapts the shared
// internal/decisionraise helper — the single validation point). A deployment
// without the seam wired answers 501, never a silent no-op.

// DecisionRaiser is the primitives-only seam the verb rides (no internal
// imports, so the host adapts the shared helper without a cycle).
type DecisionRaiser interface {
	// RequestDecision validates the ask, resolves the raising run's discussion
	// thread, and posts the inert kind='decision_request' card bound to the
	// run's work item. ErrNoRaisingThread when the run has no originating
	// thread; ErrInvalidDecisionAsk for a malformed ask.
	RequestDecision(ctx context.Context, runID, principal, workItemID string, ask DecisionAsk) (DecisionRaiseOutcome, error)
}

// DecisionAsk is the request-decision body: the ask only.
type DecisionAsk struct {
	Title       string   `json:"title"`
	Details     string   `json:"details,omitempty"`
	Options     []string `json:"options,omitempty"`
	Mode        string   `json:"mode,omitempty"`        // choose_one (default) | choose_many | approve | free_form
	Recommended int      `json:"recommended,omitempty"` // 1-based option number; 0 = none
}

// DecisionRaiseOutcome reports the posted card so the runtime can cite it.
type DecisionRaiseOutcome struct {
	MessageID string `json:"messageId"`
	ThreadID  string `json:"threadId"`
	Phase     string `json:"phase"` // "open" until the board answers
}

// Decision-raise sentinels the handler maps onto the HTTP contract.
var (
	// ErrNoRaisingThread maps to 409: the run was not dispatched from a
	// discussion thread, so there is nowhere to raise the card.
	ErrNoRaisingThread = errors.New("taskio: this run has no discussion thread to raise a decision in")
	// ErrInvalidDecisionAsk maps to 400: the ask failed the shared helper's
	// validation (missing title, <2 options on a choose mode, >9 options...).
	ErrInvalidDecisionAsk = errors.New("taskio: invalid decision ask")
)

// WithDecisionRaiser wires the decision-raise seam. Nil leaves the verb
// answering 501 (unwired), matching the nil-seam discipline of the tool
// surfaces.
func (h *Handler) WithDecisionRaiser(raise DecisionRaiser) *Handler {
	h.raise = raise
	return h
}

// requestDecision is the POST /request-decision handler: token-derived
// identity, ask-only body, shared-helper validation.
func (h *Handler) requestDecision(w http.ResponseWriter, r *http.Request, tok RunToken) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if h.raise == nil {
		writeError(w, http.StatusNotImplemented, "decision raise not wired on this deployment")
		return
	}
	var ask DecisionAsk
	if !decode(w, r, &ask) {
		return
	}
	if ask.Title == "" {
		writeError(w, http.StatusBadRequest, "title required")
		return
	}
	out, err := h.raise.RequestDecision(r.Context(), tok.RunID, tok.Principal, tok.WorkItemID, ask)
	if err != nil {
		writeDecisionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// writeDecisionError maps the seam's sentinels onto the HTTP contract.
func writeDecisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoRaisingThread):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalidDecisionAsk):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "work item not found")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
