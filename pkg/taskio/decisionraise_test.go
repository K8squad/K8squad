package taskio

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// decisionraise_test.go — ISI-7224 (WS-6.2): the run-scoped request-decision
// verb. Identity/targeting are token-derived; the body is the ask only; the
// seam's sentinels map onto the HTTP contract.

type fakeRaiser struct {
	calls    int
	lastRun  string
	lastPrin string
	lastWI   string
	lastAsk  DecisionAsk
	out      DecisionRaiseOutcome
	err      error
}

func (f *fakeRaiser) RequestDecision(_ context.Context, runID, principal, workItemID string, ask DecisionAsk) (DecisionRaiseOutcome, error) {
	f.calls++
	f.lastRun, f.lastPrin, f.lastWI, f.lastAsk = runID, principal, workItemID, ask
	return f.out, f.err
}

func TestRequestDecisionUnwiredAnswers501(t *testing.T) {
	h, m := newTestHandler(t, &fakeStore{})
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	w := do(t, h, http.MethodPost, "/request-decision", tok, `{"title":"T","options":["a","b"]}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}

func TestRequestDecisionHappyPath(t *testing.T) {
	h, m := newTestHandler(t, &fakeStore{})
	raiser := &fakeRaiser{out: DecisionRaiseOutcome{MessageID: "msg-1", ThreadID: "th-1", Phase: "open"}}
	h.WithDecisionRaiser(raiser)
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	w := do(t, h, http.MethodPost, "/request-decision", tok, `{"title":"Which?","details":"ctx","options":["a","b","c"],"mode":"choose_one","recommended":2}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if raiser.calls != 1 {
		t.Fatalf("raiser calls = %d", raiser.calls)
	}
	if raiser.lastRun != "run-A" || raiser.lastPrin != "agent" || raiser.lastWI != "wi-1" {
		t.Fatalf("identity = %s/%s/%s (must come from the token)", raiser.lastRun, raiser.lastPrin, raiser.lastWI)
	}
	if raiser.lastAsk.Title != "Which?" || len(raiser.lastAsk.Options) != 3 || raiser.lastAsk.Recommended != 2 {
		t.Fatalf("ask = %+v", raiser.lastAsk)
	}
	var got DecisionRaiseOutcome
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != raiser.out {
		t.Fatalf("outcome = %+v, want %+v", got, raiser.out)
	}
}

func TestRequestDecisionErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"no raising thread", ErrNoRaisingThread, http.StatusConflict},
		{"invalid ask", ErrInvalidDecisionAsk, http.StatusBadRequest},
		{"work item gone", ErrNotFound, http.StatusNotFound},
		{"internal", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		h, m := newTestHandler(t, &fakeStore{})
		h.WithDecisionRaiser(&fakeRaiser{err: tc.err})
		tok, _ := m.Mint("run-A", "wi-1", "agent")
		w := do(t, h, http.MethodPost, "/request-decision", tok, `{"title":"T","options":["a","b"]}`)
		if w.Code != tc.code {
			t.Fatalf("%s: status = %d, want %d", tc.name, w.Code, tc.code)
		}
	}
}

func TestRequestDecisionValidation(t *testing.T) {
	h, m := newTestHandler(t, &fakeStore{})
	raiser := &fakeRaiser{}
	h.WithDecisionRaiser(raiser)
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	if w := do(t, h, http.MethodGet, "/request-decision", tok, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/request-decision", tok, `{"options":["a","b"]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing title status = %d, want 400", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/request-decision", tok, `{"title":"T","options":["a"],"nope":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want 400", w.Code)
	}
	if raiser.calls != 0 {
		t.Fatalf("raiser must not be called on client errors, got %d", raiser.calls)
	}
	// Unauthenticated callers never reach the seam.
	if w := do(t, h, http.MethodPost, "/request-decision", "", `{"title":"T"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", w.Code)
	}
}
