package taskio

import (
	"context"
	"errors"
	"net/http"
)

// suggestraise.go — the run-scoped suggest-next-steps verb (ISI-7218,
// ISI-7075 C4, docs/board-interaction-taxonomy.md §5): the emission path a
// coordinator uses to report blocked/stuck WITH concrete proposed next actions.
// The client supplies ONLY the ask (what is blocked + the proposed steps);
// attribution (principal/run) and targeting (work item) come from the run
// token's binding, never the body — the same §AC5 discipline as every other
// verb here. The card posted is kind='suggest_next_steps' carrying the SAME
// option shape as a decision_request (one grammar, two kinds); answering rides
// the existing decision answer/reject + resume machinery unchanged.

// DecisionSuggester is the primitives-only seam the verb rides (no internal
// imports, so the host adapts the shared helper without a cycle).
type DecisionSuggester interface {
	// SuggestNextSteps validates the ask, resolves the raising run's discussion
	// thread, and posts the inert kind='suggest_next_steps' card bound to the
	// run's work item. ErrNoRaisingThread when the run has no originating
	// thread; ErrInvalidSuggestAsk for a malformed ask.
	SuggestNextSteps(ctx context.Context, runID, principal, workItemID string, ask SuggestAsk) (DecisionRaiseOutcome, error)
}

// SuggestedStepAsk is ONE proposed next action: an imperative label plus the
// one-line deciding context and the coordinator's pick flag (at most one
// recommended per card — taxonomy §5).
type SuggestedStepAsk struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// SuggestAsk is the suggest-next-steps body: the ask only. Either Steps (2–9
// concrete actions) or NoPath (the explicit free_form fallback with an
// explanation in Details) — never both.
type SuggestAsk struct {
	Title     string             `json:"title"`
	Details   string             `json:"details,omitempty"`
	Steps     []SuggestedStepAsk `json:"steps,omitempty"`
	AllowMany bool               `json:"allowMany,omitempty"`
	NoPath    bool               `json:"noPath,omitempty"`
}

// ErrInvalidSuggestAsk maps to 400: the ask failed the shared helper's
// validation (missing title, <2 steps, >9 steps, >1 recommended, noPath
// without an explanation...).
var ErrInvalidSuggestAsk = errors.New("taskio: invalid suggest-next-steps ask")

// WithDecisionSuggester wires the suggest-next-steps seam. Nil leaves the verb
// answering 501 (unwired), matching the nil-seam discipline of the tool
// surfaces.
func (h *Handler) WithDecisionSuggester(suggest DecisionSuggester) *Handler {
	h.suggester = suggest
	return h
}

// suggestNextSteps is the POST /suggest-next-steps handler: token-derived
// identity, ask-only body, shared-helper validation.
func (h *Handler) suggestNextSteps(w http.ResponseWriter, r *http.Request, tok RunToken) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if h.suggester == nil {
		writeError(w, http.StatusNotImplemented, "suggest-next-steps not wired on this deployment")
		return
	}
	var ask SuggestAsk
	if !decode(w, r, &ask) {
		return
	}
	if ask.Title == "" {
		writeError(w, http.StatusBadRequest, "title required")
		return
	}
	out, err := h.suggester.SuggestNextSteps(r.Context(), tok.RunID, tok.Principal, tok.WorkItemID, ask)
	if err != nil {
		writeSuggestError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// writeSuggestError maps the seam's sentinels onto the HTTP contract.
func writeSuggestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoRaisingThread):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalidSuggestAsk):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "work item not found")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
