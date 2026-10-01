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

// fakeGithubBridge records both legs of the bridge (find-or-create + dispatch) and
// returns canned results/errors so the handler's auth, URL→ref derivation,
// server-derived scope, idempotency pass-through and error mapping can be exercised
// without a DB or an informer cache. It satisfies GithubIssueDispatcher.
type fakeGithubBridge struct {
	ensureIn  coord.EnsureReviewWorkItemInput
	ensureRes coord.EnsureReviewWorkItemResult
	ensureErr error

	dispatchCalled bool
	dispatchIn     coord.RequestDispatchInput
	dispatchRes    coord.WorkItemDispatchResult
	dispatchErr    error
}

func (f *fakeGithubBridge) EnsureReviewWorkItem(_ context.Context, in coord.EnsureReviewWorkItemInput) (coord.EnsureReviewWorkItemResult, error) {
	f.ensureIn = in
	return f.ensureRes, f.ensureErr
}

func (f *fakeGithubBridge) RequestDispatch(_ context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error) {
	f.dispatchCalled = true
	f.dispatchIn = in
	return f.dispatchRes, f.dispatchErr
}

// testGithubBridgeServer wires the assign route with a human (alice) + agent
// session, both in teamID. No ProjectRoles/ProjectRefs is wired, so the handler
// falls back to the caller's server-derived Team scope (like the dispatch test).
func testGithubBridgeServer(t *testing.T, teamID uuid.UUID, store GithubIssueDispatcher) http.Handler {
	t.Helper()
	agentID := "agent:builder"
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken:   {Principal: "user:alice", TeamID: teamID},
		agentToken: {Principal: "agent:builder", TeamID: teamID, AgentID: &agentID},
	}}
	srv := NewServer(Options{
		Authenticator:       NewCookieAuthenticator(resolver),
		Discussion:          discussion.NewHandler(nil),
		GithubIssueDispatch: store,
	})
	return srv.Handler()
}

// fakeBridgeMirror is a GithubIssueMirrorReader double: it records the keys it
// was asked for and returns a canned mirrored issue (ISI-5308).
type fakeBridgeMirror struct {
	mi                        MirroredIssue
	ok                        bool
	err                       error
	gotNS, gotName, gotNumber string
}

func (f *fakeBridgeMirror) MirroredIssue(_ context.Context, ns, name, number string) (MirroredIssue, bool, error) {
	f.gotNS, f.gotName, f.gotNumber = ns, name, number
	return f.mi, f.ok, f.err
}

// testGithubBridgeServerFull wires the assign route with a resolver (so the
// handler learns the Project CR ns/name) and an optional mirror reader (ISI-5308
// body import). Human alice in teamID.
func testGithubBridgeServerFull(t *testing.T, teamID uuid.UUID, store GithubIssueDispatcher, refs ProjectRefResolver, mirror GithubIssueMirrorReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator:       NewCookieAuthenticator(resolver),
		Discussion:          discussion.NewHandler(nil),
		GithubIssueDispatch: store,
		ProjectRefs:         refs,
		GithubIssueMirror:   mirror,
	})
	return srv.Handler()
}

func postAssign(project, number, body, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost,
		"/api/projects/"+project+"/github/issues/"+number+"/assign", strings.NewReader(body))
	if token != "" {
		r = withSession(r, token)
	}
	return r
}

// TestGithubAssignOK — a human assigns a GitHub issue to an agent: 200, the ticket
// is created-if-absent with the derived idempotency label, then dispatched with the
// agent from the BODY and the Team/principal SERVER-DERIVED. The response carries the
// derived issueRef + created flag + dispatch states.
func TestGithubAssignOK(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	store := &fakeGithubBridge{
		ensureRes: coord.EnsureReviewWorkItemResult{Item: coord.WorkItemRecord{ID: "wi-9"}, Created: true},
		dispatchRes: coord.WorkItemDispatchResult{
			WorkItemID: "wi-9", FromState: "backlog", ToState: "todo", RequestedAgent: "coder",
		},
	}
	h := testGithubBridgeServer(t, teamID, store)

	body := `{"agentId":"coder","url":"https://github.com/K8squad/K8squad/issues/4793"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAssign("acme%2Fweb", "4793", body, devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// find-or-create leg: derived label + server-derived tenancy/principal.
	wantLabel := "ksquad.github.issue=K8squad/K8squad#4793"
	if store.ensureIn.DedupLabel != wantLabel {
		t.Fatalf("dedup label: got %q, want %q", store.ensureIn.DedupLabel, wantLabel)
	}
	if store.ensureIn.TeamID != teamID.String() || store.ensureIn.Principal != "user:alice" {
		t.Fatalf("create not server-derived: %+v", store.ensureIn)
	}
	if !strings.Contains(store.ensureIn.Body, "https://github.com/K8squad/K8squad/issues/4793") {
		t.Fatalf("ticket body must carry the issue link: %q", store.ensureIn.Body)
	}

	// dispatch leg: work item from the create, agent from the body, scope derived.
	if !store.dispatchCalled {
		t.Fatal("dispatch leg not called")
	}
	if store.dispatchIn.WorkItemID != "wi-9" || store.dispatchIn.AgentID != "coder" ||
		store.dispatchIn.TeamID != teamID.String() || store.dispatchIn.Principal != "user:alice" {
		t.Fatalf("dispatch input wrong: %+v", store.dispatchIn)
	}

	var got githubIssueAssignResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got.WorkItemID != "wi-9" || got.IssueRef != "K8squad/K8squad#4793" || !got.Created ||
		got.Dispatch.ToState != "todo" || got.Dispatch.RequestedAgent != "coder" {
		t.Fatalf("response contract: %+v", got)
	}
}

// TestGithubAssignImportsMirroredBody — ISI-5308: when the §5.4 mirror has the
// issue, the minted ticket carries the REAL upstream body + comments (keyed by
// the resolved Project CR ns/name + issue number), not the stub placeholder.
func TestGithubAssignImportsMirroredBody(t *testing.T) {
	teamID := uuid.New()
	store := &fakeGithubBridge{
		ensureRes:   coord.EnsureReviewWorkItemResult{Item: coord.WorkItemRecord{ID: "wi-1"}, Created: true},
		dispatchRes: coord.WorkItemDispatchResult{WorkItemID: "wi-1", ToState: "todo", RequestedAgent: "coder"},
	}
	refs := &fakeProjectRefs{res: ProjectRefResolution{UID: "proj-uid", TeamUID: teamID.String(), Namespace: "team-ns", Name: "widget"}}
	mirror := &fakeBridgeMirror{
		ok: true,
		mi: MirroredIssue{
			Body: "The real upstream issue body.",
			Comments: []MirroredIssueComment{
				{Actor: "alice", Body: "first reply"},
				{Actor: "bob", Body: "second reply"},
			},
		},
	}
	h := testGithubBridgeServerFull(t, teamID, store, refs, mirror)

	body := `{"agentId":"coder","url":"https://github.com/o/r/issues/7"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAssign("team-ns%2Fwidget", "7", body, devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// mirror keyed by resolved CR ns/name + issue number (NOT the UID).
	if mirror.gotNS != "team-ns" || mirror.gotName != "widget" || mirror.gotNumber != "7" {
		t.Fatalf("mirror lookup keys wrong: ns=%q name=%q number=%q", mirror.gotNS, mirror.gotName, mirror.gotNumber)
	}
	got := store.ensureIn.Body
	if !strings.Contains(got, "The real upstream issue body.") {
		t.Errorf("imported body missing the mirrored issue body: %q", got)
	}
	if !strings.Contains(got, "first reply") || !strings.Contains(got, "@alice") ||
		!strings.Contains(got, "second reply") || !strings.Contains(got, "@bob") {
		t.Errorf("imported body missing mirrored comments: %q", got)
	}
	if !strings.Contains(got, "https://github.com/o/r/issues/7") {
		t.Errorf("imported body must still carry the issue link: %q", got)
	}
}

// TestGithubAssignFallsBackToStubBody — ISI-5308: a mirror MISS (issue not synced
// yet) keeps the stub body so the ticket is still minted and dispatched.
func TestGithubAssignFallsBackToStubBody(t *testing.T) {
	teamID := uuid.New()
	store := &fakeGithubBridge{
		ensureRes:   coord.EnsureReviewWorkItemResult{Item: coord.WorkItemRecord{ID: "wi-1"}, Created: true},
		dispatchRes: coord.WorkItemDispatchResult{WorkItemID: "wi-1", ToState: "todo"},
	}
	refs := &fakeProjectRefs{res: ProjectRefResolution{UID: "proj-uid", TeamUID: teamID.String(), Namespace: "team-ns", Name: "widget"}}
	mirror := &fakeBridgeMirror{ok: false} // not mirrored yet
	h := testGithubBridgeServerFull(t, teamID, store, refs, mirror)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAssign("team-ns%2Fwidget", "7", `{"agentId":"coder","url":"https://github.com/o/r/issues/7"}`, devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got := store.ensureIn.Body
	if !strings.Contains(got, "Imported from GitHub issue: https://github.com/o/r/issues/7") ||
		!strings.Contains(got, "Assigned to an agent from the console GitHub Issues board") {
		t.Errorf("mirror miss must fall back to the stub body: %q", got)
	}
}

// TestGithubAssignIdempotentReuse — a repeat click reuses the existing ticket
// (Created=false) and still dispatches (self-heal / re-assign); the response
// faithfully reports created=false.
func TestGithubAssignIdempotentReuse(t *testing.T) {
	teamID := uuid.New()
	store := &fakeGithubBridge{
		ensureRes:   coord.EnsureReviewWorkItemResult{Item: coord.WorkItemRecord{ID: "wi-9"}, Created: false},
		dispatchRes: coord.WorkItemDispatchResult{WorkItemID: "wi-9", FromState: "todo", ToState: "todo", RequestedAgent: "coder"},
	}
	h := testGithubBridgeServer(t, teamID, store)
	body := `{"agentId":"coder","url":"https://github.com/o/r/issues/7"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAssign("p", "7", body, devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got githubIssueAssignResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Created {
		t.Fatalf("reuse must report created=false: %+v", got)
	}
	if !store.dispatchCalled {
		t.Fatal("reuse must still dispatch (self-heal / re-assign)")
	}
}

// TestGithubAssignAgentForbidden — an agent-authored session is refused 403 before
// either store leg runs (agents progress via custody, never by dispatching).
func TestGithubAssignAgentForbidden(t *testing.T) {
	store := &fakeGithubBridge{}
	h := testGithubBridgeServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAssign("p", "7", `{"agentId":"coder","url":"https://github.com/o/r/issues/7"}`, agentToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent assign: got %d, want 403", rec.Code)
	}
	if store.dispatchCalled {
		t.Fatal("store must not run for a forbidden request")
	}
}

// TestGithubAssignBadRequests — inputs that must never reach the store: missing
// agentId, an unparseable URL, and a number that disagrees with the URL (a stale
// or foreign URL must never mislabel the wrong issue).
func TestGithubAssignBadRequests(t *testing.T) {
	cases := []struct {
		name, number, body string
	}{
		{"missing-agent", "7", `{"url":"https://github.com/o/r/issues/7"}`},
		{"unparseable-url", "7", `{"agentId":"coder","url":"https://example.com/nope"}`},
		{"number-mismatch", "8", `{"agentId":"coder","url":"https://github.com/o/r/issues/7"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeGithubBridge{}
			h := testGithubBridgeServer(t, uuid.New(), store)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, postAssign("p", tc.number, tc.body, devToken))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: got %d, want 400 (body %s)", tc.name, rec.Code, rec.Body.String())
			}
			if store.dispatchCalled {
				t.Fatalf("%s: store must not run", tc.name)
			}
		})
	}
}

// TestGithubAssignErrorMapping — a sentinel from either leg maps to the board's
// shared status contract; ErrStateConflict from the dispatch leg is the "already
// assigned" 409 a re-click on a claimed issue produces.
func TestGithubAssignErrorMapping(t *testing.T) {
	body := `{"agentId":"coder","url":"https://github.com/o/r/issues/7"}`
	cases := []struct {
		name       string
		ensureErr  error
		dispatch   error
		wantStatus int
	}{
		{"already-claimed", nil, coord.ErrStateConflict, http.StatusConflict},
		{"agent-not-in-team", nil, coord.ErrAgentNotInTeam, http.StatusForbidden},
		{"create-bad-input", coord.ErrInvalidWorkItem, nil, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeGithubBridge{
				ensureRes:   coord.EnsureReviewWorkItemResult{Item: coord.WorkItemRecord{ID: "wi-9"}, Created: true},
				ensureErr:   tc.ensureErr,
				dispatchErr: tc.dispatch,
			}
			h := testGithubBridgeServer(t, uuid.New(), store)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, postAssign("p", "7", body, devToken))
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s: got %d, want %d (body %s)", tc.name, rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestGithubAssign501WithoutStore — a store-less host keeps the documented 501 so
// the console surfaces "not available in this environment" rather than a fake OK.
func TestGithubAssign501WithoutStore(t *testing.T) {
	h := testGithubBridgeServer(t, uuid.New(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAssign("p", "7", `{"agentId":"coder","url":"https://github.com/o/r/issues/7"}`, devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("store-less assign: got %d, want 501 (body %s)", rec.Code, rec.Body.String())
	}
}
