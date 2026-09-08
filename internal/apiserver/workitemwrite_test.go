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

// fakeWorkItemWriter records the last call and returns a canned result/error so the
// handlers' auth, body-parsing, and error-mapping can be exercised without a DB.
type fakeWorkItemWriter struct {
	createCalled bool
	updateCalled bool
	gotCreate    coord.CreateWorkItemInput
	gotEditID    string
	gotEdit      coord.UpdateWorkItemInput
	result       coord.WorkItemRecord
	err          error
}

func (f *fakeWorkItemWriter) CreateWorkItem(_ context.Context, in coord.CreateWorkItemInput) (coord.WorkItemRecord, error) {
	f.createCalled = true
	f.gotCreate = in
	return f.result, f.err
}

func (f *fakeWorkItemWriter) UpdateWorkItem(_ context.Context, id string, in coord.UpdateWorkItemInput) (coord.WorkItemRecord, error) {
	f.updateCalled = true
	f.gotEditID, f.gotEdit = id, in
	return f.result, f.err
}

// testWriteServer wires the create/edit routes with a human (alice) + agent session
// and a role resolver granting alice contributor on "proj-ok" and viewer on
// "proj-view" (no membership elsewhere → 404 existence-hiding). A nil store keeps
// the routes at their documented 501.
func testWriteServer(t *testing.T, teamID uuid.UUID, store WorkItemWriter) http.Handler {
	t.Helper()
	agentID := "agent:builder"
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken:   {Principal: "user:alice", TeamID: teamID},
		agentToken: {Principal: "agent:builder", TeamID: teamID, AgentID: &agentID},
	}}
	roles := fakeRoleResolver{roles: map[string]map[string]string{
		"user:alice": {"proj-ok": "contributor", "proj-view": "viewer"},
		// The agent carries a contributor row too, so it clears requireProjectRole and
		// reaches the handler's human-only gate — proving the gate, not the role wall,
		// is what refuses an agent-authored create.
		"agent:builder": {"proj-ok": "contributor"},
	}}
	srv := NewServer(Options{
		Authenticator:  NewCookieAuthenticator(resolver),
		Discussion:     discussion.NewHandler(nil),
		WorkItemWrites: store,
		ProjectRoles:   roles,
	})
	return srv.Handler()
}

func postCreate(projectID, body, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/work-items", strings.NewReader(body))
	if token != "" {
		r = withSession(r, token)
	}
	return r
}

func patchEdit(id, body, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPatch, "/api/work-items/"+id, strings.NewReader(body))
	if token != "" {
		r = withSession(r, token)
	}
	return r
}

// TestWorkItemCreateOK — a human contributor creates a root item: 201, the store is
// called with the server-derived project/team/principal (never the body).
func TestWorkItemCreateOK(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	store := &fakeWorkItemWriter{result: coord.WorkItemRecord{ID: "wi-new", ProjectID: "proj-ok", Title: "ship it", State: "backlog"}}
	h := testWriteServer(t, teamID, store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postCreate("proj-ok", `{"title":"ship it"}`, devToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.createCalled {
		t.Fatal("store not called")
	}
	if store.gotCreate.ProjectID != "proj-ok" || store.gotCreate.Title != "ship it" {
		t.Fatalf("create input: %+v", store.gotCreate)
	}
	if store.gotCreate.TeamID != teamID.String() || store.gotCreate.Principal != "user:alice" {
		t.Fatalf("identity/tenancy not server-derived: %+v", store.gotCreate)
	}
	var got coord.WorkItemRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID != "wi-new" {
		t.Fatalf("body: %v %+v", err, got)
	}
}

// TestWorkItemCreateSubIssue — parentId is forwarded so the store lands a child.
func TestWorkItemCreateSubIssue(t *testing.T) {
	store := &fakeWorkItemWriter{result: coord.WorkItemRecord{ID: "wi-child"}}
	h := testWriteServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postCreate("proj-ok", `{"title":"child","parentId":"wi-parent"}`, devToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if store.gotCreate.ParentID != "wi-parent" {
		t.Fatalf("parentId not forwarded: %+v", store.gotCreate)
	}
}

// TestWorkItemCreateAgentForbidden — an agent-authored session is refused 403 before
// the store is touched.
func TestWorkItemCreateAgentForbidden(t *testing.T) {
	store := &fakeWorkItemWriter{}
	h := testWriteServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postCreate("proj-ok", `{"title":"x"}`, agentToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent create: got %d, want 403", rec.Code)
	}
	if store.createCalled {
		t.Fatal("store must not run for a forbidden request")
	}
}

// TestWorkItemCreateViewerForbidden — a viewer (contributor wall) is 403 at the
// requireProjectRole gate, before the store.
func TestWorkItemCreateViewerForbidden(t *testing.T) {
	store := &fakeWorkItemWriter{}
	h := testWriteServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postCreate("proj-view", `{"title":"x"}`, devToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create: got %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if store.createCalled {
		t.Fatal("store must not run for an under-privileged caller")
	}
}

// TestWorkItemCreateNonMember404 — no membership ⇒ 404 existence-hiding, not 403.
func TestWorkItemCreateNonMember404(t *testing.T) {
	store := &fakeWorkItemWriter{}
	h := testWriteServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postCreate("proj-secret", `{"title":"x"}`, devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-member create: got %d, want 404", rec.Code)
	}
	if store.createCalled {
		t.Fatal("store must not run for a non-member")
	}
}

// TestWorkItemCreateBadBody — an empty title is a 400 and never calls the store.
func TestWorkItemCreateBadBody(t *testing.T) {
	store := &fakeWorkItemWriter{}
	h := testWriteServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postCreate("proj-ok", `{}`, devToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty title: got %d, want 400", rec.Code)
	}
	if store.createCalled {
		t.Fatal("store must not run without a title")
	}
}

// TestWorkItemCreateNilStore501 — no writer wired ⇒ documented 501, not 404/panic.
func TestWorkItemCreateNilStore501(t *testing.T) {
	h := testWriteServer(t, uuid.New(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postCreate("proj-ok", `{"title":"x"}`, devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil store create: got %d, want 501", rec.Code)
	}
}

// TestWorkItemEditOK — a human edits fields: 200, id + patch reach the store.
func TestWorkItemEditOK(t *testing.T) {
	teamID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	store := &fakeWorkItemWriter{result: coord.WorkItemRecord{ID: "wi-1", Title: "new title"}}
	h := testWriteServer(t, teamID, store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchEdit("wi-1", `{"title":"new title","expectedUpdatedAt":"2026-09-07T12:00:00Z"}`, devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.updateCalled || store.gotEditID != "wi-1" {
		t.Fatalf("store call: %+v id=%q", store, store.gotEditID)
	}
	if store.gotEdit.Title == nil || *store.gotEdit.Title != "new title" {
		t.Fatalf("title not forwarded: %+v", store.gotEdit)
	}
	if store.gotEdit.ExpectedUpdatedAt != "2026-09-07T12:00:00Z" {
		t.Fatalf("concurrency guard not forwarded: %+v", store.gotEdit)
	}
	if store.gotEdit.TeamID != teamID.String() || store.gotEdit.Principal != "user:alice" {
		t.Fatalf("identity/tenancy not server-derived: %+v", store.gotEdit)
	}
}

// TestWorkItemEditAgentForbidden — agents cannot edit board fields.
func TestWorkItemEditAgentForbidden(t *testing.T) {
	store := &fakeWorkItemWriter{}
	h := testWriteServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchEdit("wi-1", `{"title":"x"}`, agentToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent edit: got %d, want 403", rec.Code)
	}
	if store.updateCalled {
		t.Fatal("store must not run for a forbidden request")
	}
}

// TestWorkItemEditNoFields — a body with no editable field is a 400.
func TestWorkItemEditNoFields(t *testing.T) {
	store := &fakeWorkItemWriter{}
	h := testWriteServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchEdit("wi-1", `{}`, devToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no fields: got %d, want 400", rec.Code)
	}
	if store.updateCalled {
		t.Fatal("store must not run without a field")
	}
}

// TestWorkItemEditErrorMapping — each coord sentinel maps to its HTTP status.
func TestWorkItemEditErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid", coord.ErrInvalidWorkItem, http.StatusBadRequest},
		{"notfound", coord.ErrWorkItemNotFound, http.StatusNotFound},
		{"conflict", coord.ErrStateConflict, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := testWriteServer(t, uuid.New(), &fakeWorkItemWriter{err: tc.err})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, patchEdit("wi-1", `{"title":"x"}`, devToken))
			if rec.Code != tc.want {
				t.Fatalf("%s: got %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}
}

// TestWorkItemEditNilStore501 — no writer wired ⇒ documented 501.
func TestWorkItemEditNilStore501(t *testing.T) {
	h := testWriteServer(t, uuid.New(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchEdit("wi-1", `{"title":"x"}`, devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil store edit: got %d, want 501", rec.Code)
	}
}

// TestWorkItemEditUnauthenticated — no session ⇒ 401 at the choke point.
func TestWorkItemEditUnauthenticated(t *testing.T) {
	h := testWriteServer(t, uuid.New(), &fakeWorkItemWriter{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchEdit("wi-1", `{"title":"x"}`, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}
}
