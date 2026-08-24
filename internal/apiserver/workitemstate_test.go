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

// fakeTransitioner records the last call and returns a canned result/error so the
// handler's auth, body-parsing, and error-mapping can be exercised without a DB.
type fakeTransitioner struct {
	called bool
	gotItem, gotTeam, gotTarget, gotFrom, gotPrincipal string
	result coord.StateTransition
	err    error
}

func (f *fakeTransitioner) TransitionState(_ context.Context, item, team, target, from, principal, _ string) (coord.StateTransition, error) {
	f.called = true
	f.gotItem, f.gotTeam, f.gotTarget, f.gotFrom, f.gotPrincipal = item, team, target, from, principal
	return f.result, f.err
}

const agentToken = "dev-token-agent"

func testStateServer(t *testing.T, teamID uuid.UUID, store WorkItemStateTransitioner) http.Handler {
	t.Helper()
	agentID := "agent:builder"
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken:   {Principal: "user:alice", TeamID: teamID},
		agentToken: {Principal: "agent:builder", TeamID: teamID, AgentID: &agentID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		WorkItemState: store,
	})
	return srv.Handler()
}

func patchState(id, body, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPatch, "/api/work-items/"+id+"/state", strings.NewReader(body))
	if token != "" {
		r = withSession(r, token)
	}
	return r
}

// TestWorkItemStateHandlerOK — a human session moves a card: 200, the store is
// called with the server-derived Team + principal (never the body), and the
// transition projection is echoed back.
func TestWorkItemStateHandlerOK(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	store := &fakeTransitioner{result: coord.StateTransition{WorkItemID: "wi-1", FromState: "todo", ToState: "in_progress"}}
	h := testStateServer(t, teamID, store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchState("wi-1", `{"toState":"in_progress","fromState":"todo"}`, devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.called || store.gotItem != "wi-1" || store.gotTarget != "in_progress" || store.gotFrom != "todo" {
		t.Fatalf("store call: %+v", store)
	}
	if store.gotTeam != teamID.String() || store.gotPrincipal != "user:alice" {
		t.Fatalf("identity/tenancy not server-derived: team=%q principal=%q", store.gotTeam, store.gotPrincipal)
	}
	var got coord.StateTransition
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ToState != "in_progress" {
		t.Fatalf("body: %v %+v", err, got)
	}
}

// TestWorkItemStateHandlerAgentForbidden — an agent-authored session is refused
// 403 (human-only endpoint) BEFORE the store is touched.
func TestWorkItemStateHandlerAgentForbidden(t *testing.T) {
	teamID := uuid.New()
	store := &fakeTransitioner{}
	h := testStateServer(t, teamID, store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchState("wi-1", `{"toState":"done"}`, agentToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent PATCH: got %d, want 403", rec.Code)
	}
	if store.called {
		t.Fatal("store must not run for a forbidden request")
	}
}

// TestWorkItemStateHandlerErrorMapping — each coord sentinel maps to its HTTP status.
func TestWorkItemStateHandlerErrorMapping(t *testing.T) {
	teamID := uuid.New()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid", coord.ErrInvalidState, http.StatusBadRequest},
		{"notfound", coord.ErrWorkItemNotFound, http.StatusNotFound},
		{"conflict", coord.ErrStateConflict, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := testStateServer(t, teamID, &fakeTransitioner{err: tc.err})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, patchState("wi-1", `{"toState":"in_review"}`, devToken))
			if rec.Code != tc.want {
				t.Fatalf("%s: got %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}
}

// TestWorkItemStateHandlerBadBody — an empty toState is a 400 and never calls the store.
func TestWorkItemStateHandlerBadBody(t *testing.T) {
	store := &fakeTransitioner{}
	h := testStateServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchState("wi-1", `{}`, devToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty toState: got %d, want 400", rec.Code)
	}
	if store.called {
		t.Fatal("store must not run without a target state")
	}
}

// TestWorkItemStateHandlerUnauthenticated — no session ⇒ 401 at the choke point.
func TestWorkItemStateHandlerUnauthenticated(t *testing.T) {
	h := testStateServer(t, uuid.New(), &fakeTransitioner{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchState("wi-1", `{"toState":"done"}`, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}
}

// TestWorkItemStateHandlerNilStoreStill501 — no store wired ⇒ documented 501, not 404/panic.
func TestWorkItemStateHandlerNilStoreStill501(t *testing.T) {
	h := testStateServer(t, uuid.New(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchState("wi-1", `{"toState":"done"}`, devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil store: got %d, want 501", rec.Code)
	}
}
