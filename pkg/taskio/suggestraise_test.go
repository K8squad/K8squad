package taskio

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// suggestraise_test.go — ISI-7218 (ISI-7075 C4): the run-scoped
// suggest-next-steps verb. Identity/targeting are token-derived; the body is
// the ask only; the seam's sentinels map onto the HTTP contract.

type fakeSuggester struct {
	calls    int
	lastRun  string
	lastPrin string
	lastWI   string
	lastAsk  SuggestAsk
	out      DecisionRaiseOutcome
	err      error
}

func (f *fakeSuggester) SuggestNextSteps(_ context.Context, runID, principal, workItemID string, ask SuggestAsk) (DecisionRaiseOutcome, error) {
	f.calls++
	f.lastRun, f.lastPrin, f.lastWI, f.lastAsk = runID, principal, workItemID, ask
	return f.out, f.err
}

func TestSuggestNextStepsUnwiredAnswers501(t *testing.T) {
	h, m := newTestHandler(t, &fakeStore{})
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	w := do(t, h, http.MethodPost, "/suggest-next-steps", tok, `{"title":"T","steps":[{"label":"a"},{"label":"b"}]}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}

func TestSuggestNextStepsHappyPath(t *testing.T) {
	h, m := newTestHandler(t, &fakeStore{})
	suggester := &fakeSuggester{out: DecisionRaiseOutcome{MessageID: "msg-1", ThreadID: "th-1", Phase: "open"}}
	h.WithDecisionSuggester(suggester)
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	w := do(t, h, http.MethodPost, "/suggest-next-steps", tok,
		`{"title":"Blocked on quota","details":"parked since Monday","steps":[{"label":"Create child for quota","description":"impl owns","recommended":true},{"label":"Reassign eval","description":"reviewer owns"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if suggester.calls != 1 {
		t.Fatalf("suggester calls = %d", suggester.calls)
	}
	if suggester.lastRun != "run-A" || suggester.lastPrin != "agent" || suggester.lastWI != "wi-1" {
		t.Fatalf("identity = %s/%s/%s (must come from the token)", suggester.lastRun, suggester.lastPrin, suggester.lastWI)
	}
	if suggester.lastAsk.Title != "Blocked on quota" || len(suggester.lastAsk.Steps) != 2 || !suggester.lastAsk.Steps[0].Recommended {
		t.Fatalf("ask = %+v", suggester.lastAsk)
	}
	var got DecisionRaiseOutcome
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != suggester.out {
		t.Fatalf("outcome = %+v, want %+v", got, suggester.out)
	}
}

func TestSuggestNextStepsErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"no raising thread", ErrNoRaisingThread, http.StatusConflict},
		{"invalid ask", ErrInvalidSuggestAsk, http.StatusBadRequest},
		{"work item gone", ErrNotFound, http.StatusNotFound},
		{"internal", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		h, m := newTestHandler(t, &fakeStore{})
		h.WithDecisionSuggester(&fakeSuggester{err: tc.err})
		tok, _ := m.Mint("run-A", "wi-1", "agent")
		w := do(t, h, http.MethodPost, "/suggest-next-steps", tok, `{"title":"T","steps":[{"label":"a"},{"label":"b"}]}`)
		if w.Code != tc.code {
			t.Fatalf("%s: status = %d, want %d", tc.name, w.Code, tc.code)
		}
	}
}

func TestSuggestNextStepsValidation(t *testing.T) {
	h, m := newTestHandler(t, &fakeStore{})
	suggester := &fakeSuggester{}
	h.WithDecisionSuggester(suggester)
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	if w := do(t, h, http.MethodGet, "/suggest-next-steps", tok, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/suggest-next-steps", tok, `{"steps":[{"label":"a"},{"label":"b"}]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing title status = %d, want 400", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/suggest-next-steps", tok, `{"title":"T","nope":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want 400", w.Code)
	}
	if suggester.calls != 0 {
		t.Fatalf("suggester must not be called on client errors, got %d", suggester.calls)
	}
	// Unauthenticated callers never reach the seam.
	if w := do(t, h, http.MethodPost, "/suggest-next-steps", "", `{"title":"T"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", w.Code)
	}
}
