package taskio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeStore records the work item it was asked to act on so cross-run isolation
// can be asserted: whatever run a token is bound to, the store must only ever be
// called with THAT run's work item.
type fakeStore struct {
	detail          TaskDetail
	lastCommentWI   string
	lastAuthor      string
	lastInitialWI   string
	lastInitialRun  string
	lastInitialBody string
	lastChangeWI    string
	lastChange      ChangeRef
	lastStatusWI    string
	lastStatus      string
	fromState       string
	lastCheckoutWI  string
	fence           int64
	transitionErr   error
	fenceErr        error
	notFound        bool
}

func (f *fakeStore) GetTask(_ context.Context, wi string) (TaskDetail, error) {
	if f.notFound {
		return TaskDetail{}, ErrNotFound
	}
	d := f.detail
	d.WorkItemID = wi
	return d, nil
}

func (f *fakeStore) PostComment(_ context.Context, wi, principal, body string) (Comment, error) {
	f.lastCommentWI = wi
	f.lastAuthor = principal
	return Comment{Author: principal, Body: body, CreatedAt: time.Unix(0, 0)}, nil
}

func (f *fakeStore) PostInitialFindings(_ context.Context, wi, principal, runID, body string) (Comment, error) {
	f.lastInitialWI = wi
	f.lastAuthor = principal
	f.lastInitialRun = runID
	f.lastInitialBody = body
	return Comment{Author: principal, Body: body, CreatedAt: time.Unix(0, 0)}, nil
}

func (f *fakeStore) PostChange(_ context.Context, wi, principal, runID, kind, ref, summary string) (ChangeRef, error) {
	f.lastChangeWI = wi
	f.lastChange = ChangeRef{Kind: kind, Ref: ref, Summary: summary, Author: principal, RunID: runID, CreatedAt: time.Unix(0, 0)}
	return f.lastChange, nil
}

func (f *fakeStore) UpdateStatus(_ context.Context, wi, _, target string) (string, error) {
	f.lastStatusWI = wi
	f.lastStatus = target
	return f.fromState, f.transitionErr
}

func (f *fakeStore) Checkout(_ context.Context, wi, _, _ string) (int64, error) {
	f.lastCheckoutWI = wi
	if f.fenceErr != nil {
		return 0, f.fenceErr
	}
	return f.fence, nil
}

func newTestHandler(t *testing.T, store Store) (*Handler, *Minter) {
	t.Helper()
	m, err := NewMinter(testKey(), time.Hour)
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return NewHandler(m, store), m
}

func do(t *testing.T, h *Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	return w
}

func TestGetTaskHappyPath(t *testing.T) {
	store := &fakeStore{detail: TaskDetail{Title: "S2", Description: "seam", State: "in_progress"}}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent")

	w := do(t, h, http.MethodGet, "/get-task", tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var got TaskDetail
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WorkItemID != "wi-1" || got.Title != "S2" {
		t.Fatalf("unexpected detail: %+v", got)
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	h, _ := newTestHandler(t, &fakeStore{})
	for _, ep := range []struct{ method, path string }{
		{http.MethodGet, "/get-task"},
		{http.MethodPost, "/post-comment"},
		{http.MethodPost, "/post-change"},
		{http.MethodPost, "/update-status"},
		{http.MethodPost, "/checkout"},
	} {
		w := do(t, h, ep.method, ep.path, "", "{}")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status = %d, want 401", ep.method, ep.path, w.Code)
		}
	}
}

func TestBadTokenRejected(t *testing.T) {
	h, _ := newTestHandler(t, &fakeStore{})
	w := do(t, h, http.MethodGet, "/get-task", "not-a-jwt", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// AC5 cross-run isolation: a token bound to run-A/wi-1 can only ever drive the
// store against wi-1 — there is no request field that lets it name run-B/wi-2.
func TestCrossRunIsolation(t *testing.T) {
	store := &fakeStore{}
	h, m := newTestHandler(t, store)
	tokA, _ := m.Mint("run-A", "wi-1", "agent-A")

	// Even if a client stuffs another work item into the body, the handler uses
	// the token's binding for the store call.
	_ = do(t, h, http.MethodPost, "/post-comment", tokA, `{"body":"hi"}`)
	if store.lastCommentWI != "wi-1" || store.lastAuthor != "agent-A" {
		t.Fatalf("comment routed to %q by %q, want wi-1/agent-A", store.lastCommentWI, store.lastAuthor)
	}
	_ = do(t, h, http.MethodPost, "/checkout", tokA, "")
	if store.lastCheckoutWI != "wi-1" {
		t.Fatalf("checkout routed to %q, want wi-1", store.lastCheckoutWI)
	}
}

// ISI-5603 (ADR-0029 Option B): kind:"initial_findings" routes the same body
// through the initial-findings path, carrying the token's run for the audit
// signal's provenance — never a client-supplied run/author.
func TestPostCommentInitialFindings(t *testing.T) {
	store := &fakeStore{}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent-A")

	w := do(t, h, http.MethodPost, "/post-comment", tok, `{"body":"read the issue; plan: X","kind":"initial_findings"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if store.lastInitialWI != "wi-1" || store.lastInitialRun != "run-A" || store.lastAuthor != "agent-A" {
		t.Fatalf("initial-findings routed to wi=%q run=%q author=%q, want wi-1/run-A/agent-A",
			store.lastInitialWI, store.lastInitialRun, store.lastAuthor)
	}
	if store.lastInitialBody != "read the issue; plan: X" {
		t.Fatalf("initial-findings body = %q", store.lastInitialBody)
	}
	// The plain-comment path must NOT have been taken.
	if store.lastCommentWI != "" {
		t.Fatalf("plain PostComment also invoked (wi=%q)", store.lastCommentWI)
	}
}

// The empty (default) kind keeps the pre-5603 behaviour: a plain comment, no
// initial-findings signal.
func TestPostCommentDefaultKindUnchanged(t *testing.T) {
	store := &fakeStore{}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent-A")

	w := do(t, h, http.MethodPost, "/post-comment", tok, `{"body":"hi"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if store.lastCommentWI != "wi-1" {
		t.Fatalf("plain comment not routed (wi=%q)", store.lastCommentWI)
	}
	if store.lastInitialWI != "" {
		t.Fatalf("initial-findings path taken for default kind (wi=%q)", store.lastInitialWI)
	}
}

// An unknown kind is a closed-enum 400 — mirrors the post-change kind discipline.
func TestPostCommentUnknownKindRejected(t *testing.T) {
	store := &fakeStore{}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent-A")

	w := do(t, h, http.MethodPost, "/post-comment", tok, `{"body":"hi","kind":"bogus"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if store.lastCommentWI != "" || store.lastInitialWI != "" {
		t.Fatalf("store invoked for rejected kind (comment=%q initial=%q)", store.lastCommentWI, store.lastInitialWI)
	}
}

func TestUpdateStatusInvalidTransition(t *testing.T) {
	store := &fakeStore{transitionErr: ErrInvalidTransition}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	w := do(t, h, http.MethodPost, "/update-status", tok, `{"status":"nonsense"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

func TestUpdateStatusHappyPath(t *testing.T) {
	store := &fakeStore{}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	w := do(t, h, http.MethodPost, "/update-status", tok, `{"status":"in_review"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if store.lastStatusWI != "wi-1" || store.lastStatus != "in_review" {
		t.Fatalf("update routed to %q/%q", store.lastStatusWI, store.lastStatus)
	}
}

func TestCheckoutStaleFence(t *testing.T) {
	store := &fakeStore{fenceErr: ErrStaleFence}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	w := do(t, h, http.MethodPost, "/checkout", tok, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestCheckoutHappyPath(t *testing.T) {
	store := &fakeStore{fence: 7}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	w := do(t, h, http.MethodPost, "/checkout", tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got checkoutResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.FenceToken != 7 || got.RunID != "run-A" || got.WorkItemID != "wi-1" {
		t.Fatalf("unexpected checkout response: %+v", got)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	store := &fakeStore{notFound: true}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	w := do(t, h, http.MethodGet, "/get-task", tok, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestPostChangeHappyPath(t *testing.T) {
	store := &fakeStore{}
	h, m := newTestHandler(t, store)
	tok, _ := m.Mint("run-A", "wi-1", "agent-A")
	w := do(t, h, http.MethodPost, "/post-change", tok, `{"kind":"commit","ref":"deadbeef","summary":"fix"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	var got ChangeRef
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != "commit" || got.Ref != "deadbeef" || got.Author != "agent-A" || got.RunID != "run-A" {
		t.Fatalf("change ref = %+v", got)
	}
	// Attribution + provenance come from the token; the store saw the token's
	// work item, principal AND run — never a client-supplied author.
	if store.lastChangeWI != "wi-1" || store.lastChange.Author != "agent-A" || store.lastChange.RunID != "run-A" {
		t.Fatalf("store saw %+v on %q", store.lastChange, store.lastChangeWI)
	}
}

func TestPostChangeValidation(t *testing.T) {
	h, m := newTestHandler(t, &fakeStore{})
	tok, _ := m.Mint("run-A", "wi-1", "agent")
	for name, body := range map[string]string{
		"bad kind":   `{"kind":"wiki_edit","ref":"x"}`,
		"empty ref":  `{"kind":"commit"}`,
		"empty body": `{}`,
	} {
		w := do(t, h, http.MethodPost, "/post-change", tok, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, w.Code)
		}
	}
}
