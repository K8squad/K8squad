package apiserver

// threadworkitems_test.go — unit coverage for the ISI-7708 human thread →
// work-items mint, mirroring workitemwrite_test.go's lane: a recording fake
// stands in for coord.WorkItemWriteStore, a fake thread reader for the
// discussion Store, and the full NewServer wiring (authz + role wall) is
// exercised over real HTTP. The plan §5 invariants each get a named test:
// RBAC denial (human-only + role wall), backlog-landing (structural — the
// handler cannot set state; coord's INSERT hardcodes the entry lane, so the
// fake answers with the real contract's shape), provenance backlink present,
// and multi-draft minting.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// fakeThreadReader stands in for *discussion.Store's GetThread: it records the
// probe and answers with a canned thread or error.
type fakeThreadReader struct {
	thread     *discussion.Thread
	err        error
	gotProject string
	gotTeamID  uuid.UUID
	gotThread  uuid.UUID
}

func (f *fakeThreadReader) GetThread(_ context.Context, projectID string, teamID, threadID uuid.UUID) (*discussion.Thread, error) {
	f.gotProject, f.gotTeamID, f.gotThread = projectID, teamID, threadID
	return f.thread, f.err
}

// recordingThreadTicketer records EVERY CreateWorkItem call (the shared
// fakeWorkItemWriter keeps only the last one) and answers with records shaped
// like coord's real contract: state is ALWAYS the backlog entry lane — the
// coord INSERT hardcodes it and CreateWorkItemInput carries no State field, so
// no client of the seam can land an item anywhere else.
type recordingThreadTicketer struct {
	calls []coord.CreateWorkItemInput
	errOn int // 1-based call index that fails; 0 = never
	err   error
}

func (w *recordingThreadTicketer) CreateWorkItem(_ context.Context, in coord.CreateWorkItemInput) (coord.WorkItemRecord, error) {
	w.calls = append(w.calls, in)
	if w.errOn > 0 && len(w.calls) == w.errOn {
		return coord.WorkItemRecord{}, w.err
	}
	var prio *string
	if in.Priority != "" {
		p := in.Priority
		prio = &p
	}
	return coord.WorkItemRecord{
		ID:        fmt.Sprintf("wi-%d", len(w.calls)),
		ProjectID: in.ProjectID,
		Title:     in.Title,
		State:     "backlog", // coord contract: items are created in the entry lane
		Priority:  prio,
		Labels:    in.Labels,
	}, nil
}

func (w *recordingThreadTicketer) UpdateWorkItem(_ context.Context, _ string, _ coord.UpdateWorkItemInput) (coord.WorkItemRecord, error) {
	return coord.WorkItemRecord{}, errors.New("not used by the thread mint")
}

// fakeThreadProjectRefs resolves every ref to a fixed UID + owning Team.
type fakeThreadProjectRefs struct{ uid, team string }

func (f fakeThreadProjectRefs) ResolveProjectRef(_ context.Context, _ string) (ProjectRefResolution, error) {
	return ProjectRefResolution{UID: f.uid, TeamUID: f.team}, nil
}

// testThreadWorkItemsServer wires the mint route with a human (alice,
// contributor on "proj-ok", viewer on "proj-view") + agent session, mirroring
// testWriteServer. A nil threads seam keeps the route at its documented 501.
func testThreadWorkItemsServer(t *testing.T, teamID uuid.UUID, threads DiscussionThreadReader, store WorkItemWriter, refs ProjectRefResolver) http.Handler {
	t.Helper()
	agentID := "agent:builder"
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken:   {Principal: "user:alice", TeamID: teamID},
		agentToken: {Principal: "agent:builder", TeamID: teamID, AgentID: &agentID},
	}}
	roles := fakeRoleResolver{roles: map[string]map[string]string{
		"user:alice": {"proj-ok": "contributor", "proj-view": "viewer", "ns/demo": "contributor"},
		// The agent carries a contributor row too, so it clears the role wall
		// and reaches the handler's human-only gate — proving the gate, not
		// the role wall, is what refuses an agent-authored mint.
		"agent:builder": {"proj-ok": "contributor"},
	}}
	srv := NewServer(Options{
		Authenticator:     NewCookieAuthenticator(resolver),
		Discussion:        discussion.NewHandler(nil),
		DiscussionThreads: threads,
		WorkItemWrites:    store,
		ProjectRoles:      roles,
		ProjectRefs:       refs,
	})
	return srv.Handler()
}

func postThreadWorkItems(projectID, threadID, body, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/discussion/threads/"+threadID+"/work-items", strings.NewReader(body))
	if token != "" {
		r = withSession(r, token)
	}
	return r
}

// threadResp decodes the mint's 201 body.
type threadResp struct {
	Status   string              `json:"status"`
	ThreadID string              `json:"threadId"`
	Count    int                 `json:"count"`
	Created  []threadWorkItemRef `json:"created"`
}

// threadErrResp decodes the partial-failure 500 body.
type threadErrResp struct {
	Error   string              `json:"error"`
	Created []threadWorkItemRef `json:"created"`
}

func liveThread(projectID string, teamID, threadID uuid.UUID) *discussion.Thread {
	return &discussion.Thread{ID: threadID, ProjectID: projectID, TeamID: teamID, Title: "t", CreatedBy: "user:alice"}
}

// RBAC denial, wall #1: an agent-authored context is refused by the handler's
// human-only gate even though it clears requireProjectRole(Contributor).
func TestThreadWorkItemsAgentRefused(t *testing.T) {
	teamID, threadID := uuid.New(), uuid.New()
	threads := &fakeThreadReader{thread: liveThread("proj-ok", teamID, threadID)}
	writer := &recordingThreadTicketer{}
	h := testThreadWorkItemsServer(t, teamID, threads, writer, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postThreadWorkItems("proj-ok", threadID.String(), `{"tickets":[{"title":"x"}]}`, agentToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent mint: got %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if len(writer.calls) != 0 {
		t.Fatalf("store must not run for an agent caller, got %d calls", len(writer.calls))
	}
}

// RBAC denial, wall #2: a viewer-tier member is refused at the role wall.
func TestThreadWorkItemsViewerRefused(t *testing.T) {
	teamID, threadID := uuid.New(), uuid.New()
	threads := &fakeThreadReader{thread: liveThread("proj-view", teamID, threadID)}
	writer := &recordingThreadTicketer{}
	h := testThreadWorkItemsServer(t, teamID, threads, writer, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postThreadWorkItems("proj-view", threadID.String(), `{"tickets":[{"title":"x"}]}`, devToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer mint: got %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if len(writer.calls) != 0 {
		t.Fatal("store must not run for an under-privileged caller")
	}
}

func TestThreadWorkItemsUnauthenticated(t *testing.T) {
	teamID, threadID := uuid.New(), uuid.New()
	writer := &recordingThreadTicketer{}
	h := testThreadWorkItemsServer(t, teamID, &fakeThreadReader{thread: liveThread("proj-ok", teamID, threadID)}, writer, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postThreadWorkItems("proj-ok", threadID.String(), `{"tickets":[{"title":"x"}]}`, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous mint: got %d, want 401", rec.Code)
	}
}

// Multi-draft mint + the plan §5 invariants: every item lands in backlog (the
// coord contract — the handler structurally cannot set state), carries the
// provenance backlink labels (thread id + cited message ids), is authored by
// the authenticated human, and comes back with a board deep link.
func TestThreadWorkItemsMintsBacklogWithProvenance(t *testing.T) {
	teamID, threadID := uuid.New(), uuid.New()
	mid1, mid2 := uuid.NewString(), uuid.NewString()
	threads := &fakeThreadReader{thread: liveThread("proj-ok", teamID, threadID)}
	writer := &recordingThreadTicketer{}
	h := testThreadWorkItemsServer(t, teamID, threads, writer, nil)

	body := fmt.Sprintf(`{"tickets":[{"title":"Fix login flake"},{"title":"Harden e2e","body":"from the thread","priority":"high","sourceMessageIds":["%s","%s"]}]}`, mid1, mid2)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postThreadWorkItems("proj-ok", threadID.String(), body, devToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	var resp threadResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "created" || resp.Count != 2 || resp.ThreadID != threadID.String() {
		t.Fatalf("unexpected envelope: %+v", resp)
	}
	for _, ref := range resp.Created {
		if ref.State != "backlog" {
			t.Fatalf("created item state = %q, want backlog (entry lane, no state at create)", ref.State)
		}
	}
	if resp.Created[1].Priority == nil || *resp.Created[1].Priority != "high" {
		t.Fatalf("priority pass-through lost: %+v", resp.Created[1])
	}
	if want := "/projects/proj-ok/issues/wi-2"; resp.Created[1].URL != want {
		t.Fatalf("deep link = %q, want %q", resp.Created[1].URL, want)
	}

	if len(writer.calls) != 2 {
		t.Fatalf("want 2 creates, got %d", len(writer.calls))
	}
	first, second := writer.calls[0], writer.calls[1]
	if first.ProjectID != "proj-ok" || first.TeamID != teamID.String() || first.Principal != "user:alice" {
		t.Fatalf("tenancy/author not stamped from the server-derived context: %+v", first)
	}
	wantThreadLabel := "ksquad.discussion.thread=" + threadID.String()
	if len(first.Labels) != 1 || first.Labels[0] != wantThreadLabel {
		t.Fatalf("thread provenance label missing: %v", first.Labels)
	}
	wantLabels := []string{wantThreadLabel, "ksquad.discussion.message=" + mid1, "ksquad.discussion.message=" + mid2}
	if len(second.Labels) != len(wantLabels) {
		t.Fatalf("message provenance labels: got %v want %v", second.Labels, wantLabels)
	}
	for i, l := range wantLabels {
		if second.Labels[i] != l {
			t.Fatalf("message provenance labels: got %v want %v", second.Labels, wantLabels)
		}
	}
	// The thread probe fenced on the caller's team scope (no resolver wired).
	if threads.gotTeamID != teamID || threads.gotThread != threadID || threads.gotProject != "proj-ok" {
		t.Fatalf("thread probe scoped wrong: %+v", threads)
	}
}

// Existence-hiding: a thread outside the caller's tenancy (or absent) is a
// clean 404 and nothing is minted.
func TestThreadWorkItemsThreadNotFound(t *testing.T) {
	teamID, threadID := uuid.New(), uuid.New()
	writer := &recordingThreadTicketer{}
	h := testThreadWorkItemsServer(t, teamID, &fakeThreadReader{err: discussion.ErrThreadNotFound}, writer, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postThreadWorkItems("proj-ok", threadID.String(), `{"tickets":[{"title":"x"}]}`, devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign thread: got %d, want 404", rec.Code)
	}
	if len(writer.calls) != 0 {
		t.Fatal("store must not run for an out-of-scope thread")
	}
}

// Every draft is validated BEFORE the first create, so a bad batch mints
// nothing (no half-minted batches from client input).
func TestThreadWorkItemsValidationRefusedBeforeMint(t *testing.T) {
	teamID, threadID := uuid.New(), uuid.New()
	tooManyMsgs := make([]string, 21)
	for i := range tooManyMsgs {
		tooManyMsgs[i] = uuid.NewString()
	}
	tooManyTickets := make([]string, 21)
	for i := range tooManyTickets {
		tooManyTickets[i] = `{"title":"t"}`
	}
	cases := []struct {
		name string
		body string
	}{
		{"empty batch", `{"tickets":[]}`},
		{"missing title", `{"tickets":[{"body":"no title"}]}`},
		{"bad priority", `{"tickets":[{"title":"t","priority":"asap"}]}`},
		{"bad message id", `{"tickets":[{"title":"t","sourceMessageIds":["not-a-uuid"]}]}`},
		{"too many message refs", fmt.Sprintf(`{"tickets":[{"title":"t","sourceMessageIds":%s}]}`, threadJSON(t, tooManyMsgs))},
		{"too many tickets", fmt.Sprintf(`{"tickets":%s}`, threadJSON(t, tooManyTickets))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writer := &recordingThreadTicketer{}
			h := testThreadWorkItemsServer(t, teamID, &fakeThreadReader{thread: liveThread("proj-ok", teamID, threadID)}, writer, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, postThreadWorkItems("proj-ok", threadID.String(), tc.body, devToken))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: got %d, want 400 (body %s)", tc.name, rec.Code, rec.Body.String())
			}
			if len(writer.calls) != 0 {
				t.Fatalf("%s: nothing may be minted from an invalid batch, got %d calls", tc.name, len(writer.calls))
			}
		})
	}
}

func threadJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// With a project-ref resolver wired, the mint lands on the Project CR UID +
// the project's OWNING team (the same tenancy the console create path
// resolves), while the response deep link keeps the ROUTE ref.
func TestThreadWorkItemsRefResolution(t *testing.T) {
	teamID, threadID := uuid.New(), uuid.New()
	owningTeam := uuid.New()
	threads := &fakeThreadReader{thread: liveThread("ns/demo", owningTeam, threadID)}
	writer := &recordingThreadTicketer{}
	h := testThreadWorkItemsServer(t, teamID, threads, writer, fakeThreadProjectRefs{uid: "uid-1", team: owningTeam.String()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postThreadWorkItems("ns%2Fdemo", threadID.String(), `{"tickets":[{"title":"t"}]}`, devToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if len(writer.calls) != 1 {
		t.Fatalf("want 1 create, got %d", len(writer.calls))
	}
	if writer.calls[0].ProjectID != "uid-1" || writer.calls[0].TeamID != owningTeam.String() {
		t.Fatalf("mint must land on the resolved UID + owning team: %+v", writer.calls[0])
	}
	if threads.gotTeamID != owningTeam {
		t.Fatalf("thread probe must fence on the owning team, got %s", threads.gotTeamID)
	}
	var resp threadResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := "/projects/ns%2Fdemo/issues/wi-1"; resp.Created[0].URL != want {
		t.Fatalf("deep link = %q, want %q (route ref, ISI-7221)", resp.Created[0].URL, want)
	}
}

// A mid-batch infrastructure failure reports the items that DID land so the
// caller can retry the remainder — never a silent partial success.
func TestThreadWorkItemsPartialFailureReportsLanded(t *testing.T) {
	teamID, threadID := uuid.New(), uuid.New()
	writer := &recordingThreadTicketer{errOn: 3, err: errors.New("db down")}
	h := testThreadWorkItemsServer(t, teamID, &fakeThreadReader{thread: liveThread("proj-ok", teamID, threadID)}, writer, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postThreadWorkItems("proj-ok", threadID.String(), `{"tickets":[{"title":"a"},{"title":"b"},{"title":"c"}]}`, devToken))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("partial failure: got %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
	var resp threadErrResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Created) != 2 || resp.Error == "" {
		t.Fatalf("partial failure must report the 2 landed items + the error: %+v", resp)
	}
}

// A host without the thread seam keeps the route at its documented 501.
func TestThreadWorkItemsNotImplemented(t *testing.T) {
	teamID, threadID := uuid.New(), uuid.New()
	h := testThreadWorkItemsServer(t, teamID, nil, &recordingThreadTicketer{}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postThreadWorkItems("proj-ok", threadID.String(), `{"tickets":[{"title":"x"}]}`, devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("seamless host: got %d, want 501", rec.Code)
	}
}
