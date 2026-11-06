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
	gotParent  string
	list       []coord.BoardItem

	threadCalled bool
	gotThreadID  string
	thread       coord.WorkItemThread

	activityCalled bool
	gotActivityID  string
	activity       []coord.TaskComment

	listErr     error
	threadErr   error
	activityErr error
}

func (f *fakeWorkItemReader) ListWorkItems(_ context.Context, teamID, projectID, parentID string) ([]coord.BoardItem, error) {
	f.listCalled = true
	f.gotTeam, f.gotProject, f.gotParent = teamID, projectID, parentID
	return f.list, f.listErr
}

func (f *fakeWorkItemReader) ReadWorkItemThread(_ context.Context, workItemID, teamID string) (coord.WorkItemThread, error) {
	f.threadCalled = true
	f.gotThreadID, f.gotTeam = workItemID, teamID
	return f.thread, f.threadErr
}

func (f *fakeWorkItemReader) ReadWorkItemActivity(_ context.Context, workItemID, teamID string) ([]coord.TaskComment, error) {
	f.activityCalled = true
	f.gotActivityID, f.gotTeam = workItemID, teamID
	return f.activity, f.activityErr
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

// TestWorkItemListForwardsParentID — ISI-4536: the 8.17 lazy-load's
// ?parentId={id} reaches the store so the console's sub-ticket card gets
// DIRECT children, never the whole-Project card list.
func TestWorkItemListForwardsParentID(t *testing.T) {
	parent := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	store := &fakeWorkItemReader{list: []coord.BoardItem{}}
	h := testReadServer(t, uuid.New(), store)

	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/projects/proj-ok/work-items?parentId="+parent.String(), nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.listCalled || store.gotParent != parent.String() {
		t.Fatalf("store call: called=%v parent=%q", store.listCalled, store.gotParent)
	}
}

// TestWorkItemListRejectsBadParentID — a malformed parentId is a client error
// (400), never a 502 "read unavailable" from a failed SQL cast.
func TestWorkItemListRejectsBadParentID(t *testing.T) {
	store := &fakeWorkItemReader{}
	h := testReadServer(t, uuid.New(), store)

	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/projects/proj-ok/work-items?parentId=not-a-uuid", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if store.listCalled {
		t.Fatal("store must not be called on a malformed parentId")
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

// TestWorkItemReadsNotWired — a nil reader keeps the read GETs at the documented
// 501 (a DB-less dev run), exactly like the other read models — including the
// two-phase Activity feed (ISI-7095), in lockstep with the thread route.
func TestWorkItemReadsNotWired(t *testing.T) {
	h := testReadServer(t, uuid.New(), nil)
	for _, path := range []string{"/api/projects/proj-ok/work-items", "/api/work-items/wi-1", "/api/work-items/wi-1/activity"} {
		rec := httptest.NewRecorder()
		req := withSession(httptest.NewRequest(http.MethodGet, path, nil), devToken)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: got %d, want 501", path, rec.Code)
		}
	}
}

// TestWorkItemActivityOK — the two-phase Activity feed (ISI-7095) returns the
// per-run PROGRESS rows as a JSON array, Team-scoped, each carrying its kind +
// run id so the console groups the expander per run.
func TestWorkItemActivityOK(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	store := &fakeWorkItemReader{activity: []coord.TaskComment{
		{Author: "run/abcd1234", Body: "[tool:edit/result(ok)] wired the seam", Kind: "progress", RunID: "abcd1234-0000-0000-0000-000000000000"},
		{Author: "run/abcd1234", Body: "[status] in_review", Kind: "progress", RunID: "abcd1234-0000-0000-0000-000000000000"},
	}}
	h := testReadServer(t, teamID, store)

	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/work-items/wi-1/activity", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.activityCalled || store.gotActivityID != "wi-1" || store.gotTeam != teamID.String() {
		t.Fatalf("store call: called=%v id=%q team=%q", store.activityCalled, store.gotActivityID, store.gotTeam)
	}
	var got []coord.TaskComment
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 2 {
		t.Fatalf("body: %v %+v", err, got)
	}
	if got[0].Kind != "progress" || got[0].RunID == "" {
		t.Fatalf("progress row lost its kind/runId: %+v", got[0])
	}
}

// TestWorkItemActivityEmptyIsArray — a run with no mirrored progress renders
// `[]`, never `null`, so the console maps the expander unconditionally.
func TestWorkItemActivityEmptyIsArray(t *testing.T) {
	store := &fakeWorkItemReader{}
	h := testReadServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/work-items/wi-1/activity", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if b := rec.Body.String(); b == "null" || b == "" {
		t.Fatalf("empty activity must be [], got %q", b)
	}
}

// TestWorkItemActivityNotFound — the store's tenancy sentinel maps to 404
// (existence-hiding), the same fence the thread read applies.
func TestWorkItemActivityNotFound(t *testing.T) {
	store := &fakeWorkItemReader{activityErr: coord.ErrWorkItemNotFound}
	h := testReadServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/work-items/wi-gone/activity", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

// TestWorkItemActivityUnauthenticated — behind the §13 choke point: no session,
// no progress feed.
func TestWorkItemActivityUnauthenticated(t *testing.T) {
	h := testReadServer(t, uuid.New(), &fakeWorkItemReader{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/work-items/wi-1/activity", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

// fakeProjectRefs records the ref it was asked to resolve and returns a canned
// ProjectRefResolution (or error), standing in for the informer-cache resolver.
type fakeProjectRefs struct {
	gotRef string
	res    ProjectRefResolution
	err    error
}

func (f *fakeProjectRefs) ResolveProjectRef(_ context.Context, ref string) (ProjectRefResolution, error) {
	f.gotRef = ref
	return f.res, f.err
}

// testReadServerWithRefs wires the read routes with the project-ref resolver
// (the production console shape: "ns/name" ids resolved to Project CR UIDs).
func testReadServerWithRefs(t *testing.T, teamID uuid.UUID, admin bool, store WorkItemReader, refs ProjectRefResolver) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID, IsAdmin: admin},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		WorkItemReads: store,
		ProjectRefs:   refs,
	})
	return srv.Handler()
}

// TestWorkItemListResolvesConsoleID — the console's "ns/name" project id is
// resolved to the Project CR UID before the store is called (ISI-4132): without
// this the Postgres store's uuid cast fails and the Issues tab dies with a 502.
func TestWorkItemListResolvesConsoleID(t *testing.T) {
	store := &fakeWorkItemReader{list: []coord.BoardItem{{ID: "wi-1", Title: "ship", State: "todo"}}}
	refs := &fakeProjectRefs{res: ProjectRefResolution{UID: "912e88e2-7f56-4d46-8a81-f2eab0019421", TeamUID: "7191cc8c-f4b7-4b60-b63e-d25408ac0d1c"}}
	h := testReadServerWithRefs(t, uuid.New(), false, store, refs)

	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/projects/bmad-squad%2Fbmad-demo-project/work-items", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if refs.gotRef != "bmad-squad/bmad-demo-project" {
		t.Fatalf("resolver got %q, want the decoded ns/name id", refs.gotRef)
	}
	if store.gotProject != "912e88e2-7f56-4d46-8a81-f2eab0019421" {
		t.Fatalf("store got project %q, want the resolved Project CR UID", store.gotProject)
	}
}

// TestWorkItemListAdminSeesFleet — a global admin's dangling bootstrap Team
// (ISI-3921) must NOT fence the board to emptiness: the store gets the trusted
// unscoped "" team so every squad's cards are visible (ISI-4132).
func TestWorkItemListAdminSeesFleet(t *testing.T) {
	store := &fakeWorkItemReader{list: []coord.BoardItem{{ID: "wi-1"}}}
	h := testReadServerWithRefs(t, uuid.New(), true, store, nil)

	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/projects/proj-ok/work-items", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if store.gotTeam != "" {
		t.Fatalf("admin store team = %q, want unscoped \"\"", store.gotTeam)
	}
}

// TestWorkItemListUnknownProject404 — an unresolvable ref is existence-hiding
// 404, indistinguishable from a foreign Project (NFR-SEC5).
func TestWorkItemListUnknownProject404(t *testing.T) {
	store := &fakeWorkItemReader{}
	refs := &fakeProjectRefs{err: ErrProjectNotFound}
	h := testReadServerWithRefs(t, uuid.New(), false, store, refs)
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/projects/nope%2Fgone/work-items", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	if store.listCalled {
		t.Fatal("store must not be called for an unresolved project")
	}
}

// TestWorkItemListAmbiguousProject409 — a bare name colliding across squads is
// 409 (address by UID), never a silent first-match (same contract as the
// dashboard fleet resolver).
func TestWorkItemListAmbiguousProject409(t *testing.T) {
	refs := &fakeProjectRefs{err: ErrProjectAmbiguous}
	h := testReadServerWithRefs(t, uuid.New(), false, &fakeWorkItemReader{}, refs)
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/projects/shared-name/work-items", nil), devToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", rec.Code)
	}
}
