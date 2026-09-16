package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// fakeDispatcher records the last RequestDispatch call and returns a canned
// result/error so the handler's auth, body-parsing, server-derived scope, and
// error-mapping can be exercised without a DB or an informer cache.
type fakeDispatcher struct {
	called bool
	got    coord.RequestDispatchInput
	result coord.WorkItemDispatchResult
	err    error
}

func (f *fakeDispatcher) RequestDispatch(_ context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error) {
	f.called = true
	f.got = in
	return f.result, f.err
}

// testDispatchServer wires the dispatch route with a human (alice) + agent
// session, both in teamID. The route is keyed by item id (no project role wall),
// so tenancy is the store's Team fence — the handler's job here is the human-only
// gate, body parsing, server-derived Team scope, and error mapping.
func testDispatchServer(t *testing.T, teamID uuid.UUID, store WorkItemDispatcher) http.Handler {
	t.Helper()
	agentID := "agent:builder"
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken:   {Principal: "user:alice", TeamID: teamID},
		agentToken: {Principal: "agent:builder", TeamID: teamID, AgentID: &agentID},
	}}
	srv := NewServer(Options{
		Authenticator:    NewCookieAuthenticator(resolver),
		Discussion:       discussion.NewHandler(nil),
		WorkItemDispatch: store,
	})
	return srv.Handler()
}

func postDispatch(id, body, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/work-items/"+id+"/dispatch", strings.NewReader(body))
	if token != "" {
		r = withSession(r, token)
	}
	return r
}

// TestWorkItemDispatchOK — a human dispatches an item to an agent: 200, and the
// store is called with the item id from the PATH, the agent from the BODY, and
// the Team scope + principal SERVER-DERIVED (never trusted from the body).
func TestWorkItemDispatchOK(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	store := &fakeDispatcher{result: coord.WorkItemDispatchResult{
		WorkItemID: "wi-1", FromState: "backlog", ToState: "todo", RequestedAgent: "coder",
	}}
	h := testDispatchServer(t, teamID, store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postDispatch("wi-1", `{"agentId":"coder"}`, devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.called {
		t.Fatal("store not called")
	}
	if store.got.WorkItemID != "wi-1" || store.got.AgentID != "coder" {
		t.Fatalf("path/body not forwarded: %+v", store.got)
	}
	if store.got.TeamID != teamID.String() || store.got.Principal != "user:alice" {
		t.Fatalf("identity/tenancy not server-derived: %+v", store.got)
	}
	var got coord.WorkItemDispatchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ToState != "todo" || got.RequestedAgent != "coder" {
		t.Fatalf("body: %v %+v", err, got)
	}
}

// TestWorkItemDispatchAgentForbidden — an agent-authored session is refused 403
// before the store is touched (agents progress via custody, never by dispatch).
func TestWorkItemDispatchAgentForbidden(t *testing.T) {
	store := &fakeDispatcher{}
	h := testDispatchServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postDispatch("wi-1", `{"agentId":"coder"}`, agentToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent dispatch: got %d, want 403", rec.Code)
	}
	if store.called {
		t.Fatal("store must not run for a forbidden request")
	}
}

// TestWorkItemReassignOK — the ISI-4573 todo re-assign rides the SAME route and
// verb: success stays 200 (not 201) with the result body naming the states —
// for a re-assign fromState==toState=="todo" — and the store input is
// indistinguishable from a dispatch (the branch is store-internal).
func TestWorkItemReassignOK(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	store := &fakeDispatcher{result: coord.WorkItemDispatchResult{
		WorkItemID: "wi-1", FromState: "todo", ToState: "todo", RequestedAgent: "reviewer",
	}}
	h := testDispatchServer(t, teamID, store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postDispatch("wi-1", `{"agentId":"reviewer"}`, devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-assign: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.called {
		t.Fatal("store not called")
	}
	if store.got.AgentID != "reviewer" || store.got.TeamID != teamID.String() {
		t.Fatalf("store input wrong for re-assign: %+v", store.got)
	}
	var got coord.WorkItemDispatchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got.FromState != "todo" || got.ToState != "todo" || got.RequestedAgent != "reviewer" {
		t.Fatalf("re-assign body must report todo→todo with the swapped agent: %+v", got)
	}
}

// TestWorkItemDispatchMissingAgentID — no agentId ⇒ 400 before the store.
func TestWorkItemDispatchMissingAgentID(t *testing.T) {
	store := &fakeDispatcher{}
	h := testDispatchServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postDispatch("wi-1", `{}`, devToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing agentId: got %d, want 400", rec.Code)
	}
	if store.called {
		t.Fatal("store must not run without an agentId")
	}
}

// TestWorkItemDispatchErrorMapping — each store sentinel maps to the board's
// shared status contract, including the §D4 agent-∈-Team 403 dispatch adds.
func TestWorkItemDispatchErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"agent-not-in-team", coord.ErrAgentNotInTeam, http.StatusForbidden},
		{"wrong-lane", coord.ErrStateConflict, http.StatusConflict},
		{"missing-item", coord.ErrWorkItemNotFound, http.StatusNotFound},
		{"bad-input", coord.ErrInvalidWorkItem, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeDispatcher{err: tc.err}
			h := testDispatchServer(t, uuid.New(), store)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, postDispatch("wi-1", `{"agentId":"coder"}`, devToken))
			if rec.Code != tc.want {
				t.Fatalf("%s: got %d, want %d (body %s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestWorkItemDispatch501WithoutStore — a store-less host keeps the documented
// 501 rather than pretending to dispatch.
func TestWorkItemDispatch501WithoutStore(t *testing.T) {
	h := testDispatchServer(t, uuid.New(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postDispatch("wi-1", `{"agentId":"coder"}`, devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("store-less dispatch: got %d, want 501 (body %s)", rec.Code, rec.Body.String())
	}
}
