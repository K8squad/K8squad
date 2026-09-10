package apiserver

// workitemread_test.go — the M1.5 board read handlers (ISI-4131): auth, team
// scoping derivation, JSON shape, and the documented-501 host shape. The
// database-backed properties (SQL text, tenancy 404, status-history tail) ride
// the coord/taskio integration lanes; here a fake store pins the shell.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// fakeWorkItemReader records the last call and returns canned results/errors.
type fakeWorkItemReader struct {
	listCalled bool
	gotTeam    string
	gotProject string
	list       []coord.BoardItem

	threadCalled bool
	gotThreadID  string
	thread       coord.WorkItemThread

	listErr   error
	threadErr error
}

func (f *fakeWorkItemReader) ListWorkItems(_ context.Context, teamID, projectID string) ([]coord.BoardItem, error) {
	f.listCalled = true
	f.gotTeam, f.gotProject = teamID, projectID
	return f.list, f.listErr
}

func (f *fakeWorkItemReader) ReadWorkItemThread(_ context.Context, workItemID, teamID string) (coord.WorkItemThread, error) {
	f.threadCalled = true
	f.gotThreadID, f.gotTeam = workItemID, teamID
	return f.thread, f.threadErr
}

// testReadServer wires the two M1.5 read routes with a human (alice) session on
// teamID. A nil store keeps the routes at their documented 501.
func testReadServer(t *testing.T, teamID uuid.UUID, store WorkItemReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		WorkItemReads: store,
	})
	return srv.Handler()
}

// TestWorkItemListOK — the board card list returns 200 with a JSON ARRAY (never
// null) and the store is scoped by the server-derived team + path project.
func TestWorkItemListOK(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	store := &fakeWorkItemReader{list: []coord.BoardItem{{ID: "wi-1", Title: "ship", State: "in_progress", Holder: "agent:builder", CommentCount: 2, ChangeCount: 1}}}
	h := testReadServer(t, teamID, store)

	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/projects/proj-ok/work-items", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.listCalled || store.gotProject != "proj-ok" || store.gotTeam != teamID.String() {
		t.Fatalf("store call: called=%v project=%q team=%q", store.listCalled, store.gotProject, store.gotTeam)
	}
	var got []coord.BoardItem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].ID != "wi-1" {
		t.Fatalf("body: %v %+v", err, got)
	}
}

// TestWorkItemListEmptyIsArray — an empty board renders `[]`, not `null`, so the
// console (M1.6) can map over it unconditionally.
func TestWorkItemListEmptyIsArray(t *testing.T) {
	store := &fakeWorkItemReader{}
	h := testReadServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/projects/proj-ok/work-items", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if rec.Body.String() == "null" || rec.Body.String() == "" {
		t.Fatalf("empty board must be [], got %q", rec.Body.String())
	}
}

// TestWorkItemListUnauthenticated — behind the §13 choke point: no session, no list.
func TestWorkItemListUnauthenticated(t *testing.T) {
	h := testReadServer(t, uuid.New(), &fakeWorkItemReader{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/proj-ok/work-items", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

// TestWorkItemThreadOK — the ticket thread returns the full M1.5 payload:
// comments, status history, change refs — one GET, no human relay.
func TestWorkItemThreadOK(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	store := &fakeWorkItemReader{thread: coord.WorkItemThread{
		TaskDetail: coord.TaskDetail{
			WorkItemID: "wi-1",
			Title:      "ship it",
			State:      "in_review",
			Comments:   []coord.TaskComment{{Author: "agent:builder", Body: "progress: seam wired"}},
			ChangeRefs: []coord.ChangeRef{{Kind: "commit", Ref: "deadbeef", Author: "agent:builder"}},
		},
		StatusHistory: []coord.StatusChange{{FromState: "in_progress", ToState: "in_review", Principal: "agent:builder"}},
	}}
	h := testReadServer(t, teamID, store)

	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/work-items/wi-1", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.threadCalled || store.gotThreadID != "wi-1" || store.gotTeam != teamID.String() {
		t.Fatalf("store call: called=%v id=%q team=%q", store.threadCalled, store.gotThreadID, store.gotTeam)
	}
	var got coord.WorkItemThread
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Title != "ship it" || len(got.Comments) != 1 || len(got.ChangeRefs) != 1 || len(got.StatusHistory) != 1 {
		t.Fatalf("thread = %+v", got)
	}
	if got.StatusHistory[0].ToState != "in_review" || got.ChangeRefs[0].Ref != "deadbeef" {
		t.Fatalf("history/changes = %+v / %+v", got.StatusHistory, got.ChangeRefs)
	}
}

// TestWorkItemThreadNotFound — the store's tenancy sentinel maps to 404
// (existence-hiding), the same as the write surface.
func TestWorkItemThreadNotFound(t *testing.T) {
	store := &fakeWorkItemReader{threadErr: coord.ErrWorkItemNotFound}
	h := testReadServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/work-items/wi-gone", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

// TestWorkItemReadsNotWired — a nil reader keeps BOTH GETs at the documented
// 501 (a DB-less dev run), exactly like the other read models.
func TestWorkItemReadsNotWired(t *testing.T) {
	h := testReadServer(t, uuid.New(), nil)
	for _, path := range []string{"/api/projects/proj-ok/work-items", "/api/work-items/wi-1"} {
		rec := httptest.NewRecorder()
		req := withSession(httptest.NewRequest(http.MethodGet, path, nil), devToken)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: got %d, want 501", path, rec.Code)
		}
	}
}
