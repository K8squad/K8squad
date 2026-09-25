package apiserver

// ISI-4928 — proposal confirm/dismiss shell tests. The room never executes: these prove the
// shells fan ONLY into the existing authoring seams (WorkItemWriter / WorkItemDispatcher),
// refuse agents, map lifecycle sentinels (404/409), roll back on fan-out failure, and post the
// executed result back through the lifecycle store.

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

// ---- fakes -------------------------------------------------------------------

// fakeProposalLifecycle records every lifecycle call; its answers are per-test configurable.
type fakeProposalLifecycle struct {
	confirmErr  error
	dismissErr  error
	complete    *discussion.Message
	completeErr error
	payload     discussion.ProposalPayload // the proposal served after a successful CAS

	confirmCalled  bool
	confirmGot     [4]uuid.UUID
	dismissCalled  bool
	dismissGot     [4]uuid.UUID
	completeCalled bool
	completeGot    [4]uuid.UUID
	completeResult json.RawMessage
	completeBody   string
	failCalled     bool
	failGot        [3]uuid.UUID
}

func (f *fakeProposalLifecycle) ConfirmProposal(_ context.Context, projectID, teamID, messageID uuid.UUID, _ discussion.AuthorContext) (*discussion.Proposal, error) {
	f.confirmCalled = true
	f.confirmGot = [4]uuid.UUID{projectID, teamID, messageID}
	if f.confirmErr != nil {
		return nil, f.confirmErr
	}
	return &discussion.Proposal{
		Message: discussion.Message{ID: messageID, ThreadID: uuid.New(), Body: "propose"},
		TeamID:  teamID,
		Payload: f.payload,
		Phase:   discussion.ProposalPhaseConfirmed,
	}, nil
}

func (f *fakeProposalLifecycle) DismissProposal(_ context.Context, projectID, teamID, messageID uuid.UUID, _ discussion.AuthorContext) error {
	f.dismissCalled = true
	f.dismissGot = [4]uuid.UUID{projectID, teamID, messageID}
	return f.dismissErr
}

func (f *fakeProposalLifecycle) CompleteProposal(_ context.Context, projectID, teamID, messageID uuid.UUID, _ discussion.AuthorContext, result json.RawMessage, resultBody string) (*discussion.Message, error) {
	f.completeCalled = true
	f.completeGot = [4]uuid.UUID{projectID, teamID, messageID}
	f.completeResult = result
	f.completeBody = resultBody
	if f.completeErr != nil {
		return nil, f.completeErr
	}
	if f.complete == nil {
		return &discussion.Message{ID: uuid.New(), ParentID: &messageID, Body: resultBody}, nil
	}
	return f.complete, nil
}

func (f *fakeProposalLifecycle) FailProposal(_ context.Context, projectID, teamID, messageID uuid.UUID) error {
	f.failCalled = true
	f.failGot = [3]uuid.UUID{projectID, teamID, messageID}
	return nil
}

// fakeDispatcher lives in workitemdispatch_test.go — same package, reused here.

// ---- harness -----------------------------------------------------------------

const (
	projUUID   = "11111111-1111-1111-1111-111111111111"
	viewedUUID = "22222222-2222-2222-2222-222222222222"
)

func testProposalServer(t *testing.T, teamID uuid.UUID, lifecycle ProposalLifecycle, writer WorkItemWriter, dispatcher WorkItemDispatcher) http.Handler {
	t.Helper()
	agentID := "agent:builder"
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken:   {Principal: "user:alice", TeamID: teamID},
		agentToken: {Principal: "agent:builder", TeamID: teamID, AgentID: &agentID},
	}}
	roles := fakeRoleResolver{roles: map[string]map[string]string{
		"user:alice":    {projUUID: "contributor", viewedUUID: "viewer"},
		"agent:builder": {projUUID: "contributor"},
	}}
	srv := NewServer(Options{
		Authenticator:       NewCookieAuthenticator(resolver),
		Discussion:          discussion.NewHandler(nil),
		DiscussionProposals: lifecycle,
		WorkItemWrites:      writer,
		WorkItemDispatch:    dispatcher,
		ProjectRoles:        roles,
	})
	return srv.Handler()
}

func postProposalVerb(projectID, messageID, verb, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost,
		"/api/projects/"+projectID+"/discussion/proposals/"+messageID+"/"+verb, nil)
	if token != "" {
		r = withSession(r, token)
	}
	return r
}

// ---- confirm -----------------------------------------------------------------

// TestProposalConfirmCreateTicket — create_ticket fans into the CREATE seam with the payload's
// title/body and the server-derived principal + room team; the result post-back carries the new
// work item id; the card ends executed.
func TestProposalConfirmCreateTicket(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	life := &fakeProposalLifecycle{}
	life.payload = discussion.ProposalPayload{Action: discussion.ProposalActionCreateTicket, Title: "ship it", Body: "the body"}
	writer := &fakeWorkItemWriter{result: coord.WorkItemRecord{ID: "wi-1", State: "backlog"}}
	h := testProposalServer(t, teamID, life, writer, &fakeDispatcher{})

	msgID := uuid.NewString()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, msgID, "confirm", devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !writer.createCalled {
		t.Fatal("confirm did not fan into the create seam")
	}
	if writer.gotCreate.Title != "ship it" || writer.gotCreate.Body != "the body" {
		t.Fatalf("create input: %+v", writer.gotCreate)
	}
	if writer.gotCreate.TeamID != teamID.String() || writer.gotCreate.Principal != "user:alice" {
		t.Fatalf("identity/tenancy not server-derived: %+v", writer.gotCreate)
	}
	if !life.completeCalled {
		t.Fatal("executed result was not posted back through the lifecycle")
	}
	var res map[string]any
	if err := json.Unmarshal(life.completeResult, &res); err != nil {
		t.Fatalf("result json: %v", err)
	}
	if res["workItemId"] != "wi-1" || res["action"] != discussion.ProposalActionCreateTicket {
		t.Fatalf("result payload: %+v", res)
	}
	if !strings.Contains(life.completeBody, "wi-1") {
		t.Fatalf("post-back body should name the work item: %q", life.completeBody)
	}
}

// TestProposalConfirmAssignAgent — assign_agent fans into the DISPATCH seam with the payload's
// ticket + agent; the create seam stays untouched.
func TestProposalConfirmAssignAgent(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	life := &fakeProposalLifecycle{}
	life.payload = discussion.ProposalPayload{Action: discussion.ProposalActionAssignAgent, TicketID: "wi-7", AssigneeAgentID: "agent:kimi"}
	writer := &fakeWorkItemWriter{}
	disp := &fakeDispatcher{result: coord.WorkItemDispatchResult{WorkItemID: "wi-7", FromState: "backlog", ToState: "todo", RequestedAgent: "agent:kimi"}}
	h := testProposalServer(t, teamID, life, writer, disp)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, uuid.NewString(), "confirm", devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !disp.called {
		t.Fatal("confirm did not fan into the dispatch seam")
	}
	if disp.got.WorkItemID != "wi-7" || disp.got.AgentID != "agent:kimi" || disp.got.TeamID != teamID.String() {
		t.Fatalf("dispatch input: %+v", disp.got)
	}
	if writer.createCalled {
		t.Fatal("assign_agent must not mint a work item")
	}
}

// TestProposalConfirmPartyRun — party_run mints a TEAM-TARGETED item through the create seam
// (team set, no dispatch, no requested_agent — plan §6) and the result is labelled teamRun.
func TestProposalConfirmPartyRun(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	life := &fakeProposalLifecycle{}
	life.payload = discussion.ProposalPayload{Action: discussion.ProposalActionPartyRun, Title: "party: sweep the backlog"}
	writer := &fakeWorkItemWriter{result: coord.WorkItemRecord{ID: "wi-party", State: "backlog"}}
	disp := &fakeDispatcher{}
	h := testProposalServer(t, teamID, life, writer, disp)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, uuid.NewString(), "confirm", devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !writer.createCalled || disp.called {
		t.Fatalf("party_run must fan to create only (create=%v dispatch=%v)", writer.createCalled, disp.called)
	}
	if writer.gotCreate.TeamID != teamID.String() {
		t.Fatalf("party mint must carry the team: %+v", writer.gotCreate)
	}
	var res map[string]any
	if err := json.Unmarshal(life.completeResult, &res); err != nil {
		t.Fatalf("result json: %v", err)
	}
	if res["teamRun"] != true {
		t.Fatalf("party result must be labelled teamRun: %+v", res)
	}
}

// TestProposalConfirmAgentForbidden — an agent may propose, never authorize: its AuthorContext
// is refused at the human-only wall before any lifecycle or authoring call.
func TestProposalConfirmAgentForbidden(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	life := &fakeProposalLifecycle{}
	writer := &fakeWorkItemWriter{}
	h := testProposalServer(t, teamID, life, writer, &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, uuid.NewString(), "confirm", agentToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if life.confirmCalled || writer.createCalled {
		t.Fatal("agent confirm must not reach the lifecycle or the authoring seams")
	}
}

// TestProposalConfirmUnauthenticated — no session ⇒ 401 at the choke point.
func TestProposalConfirmUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	h := testProposalServer(t, teamID, &fakeProposalLifecycle{}, &fakeWorkItemWriter{}, &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, uuid.NewString(), "confirm", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

// TestProposalConfirmViewerRefused — the confirm verb rides requireProjectRole(Contributor),
// the same capability gate as the create/dispatch verbs it mirrors.
func TestProposalConfirmViewerRefused(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	life := &fakeProposalLifecycle{}
	h := testProposalServer(t, teamID, life, &fakeWorkItemWriter{}, &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(viewedUUID, uuid.NewString(), "confirm", devToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 (viewer must not confirm)", rec.Code)
	}
	if life.confirmCalled {
		t.Fatal("viewer confirm must not reach the lifecycle")
	}
}

// TestProposalConfirmAlreadyDecided — a card that left `proposed` answers 409 and nothing fans out.
func TestProposalConfirmAlreadyDecided(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	life := &fakeProposalLifecycle{confirmErr: discussion.ErrProposalNotProposed}
	writer := &fakeWorkItemWriter{}
	h := testProposalServer(t, teamID, life, writer, &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, uuid.NewString(), "confirm", devToken))
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if writer.createCalled || life.completeCalled {
		t.Fatal("an already-decided card must not execute anything")
	}
}

// TestProposalConfirmNotFound — cross-tenant / absent proposals are 404 (existence-hiding).
func TestProposalConfirmNotFound(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	life := &fakeProposalLifecycle{confirmErr: discussion.ErrProposalNotFound}
	h := testProposalServer(t, teamID, life, &fakeWorkItemWriter{}, &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, uuid.NewString(), "confirm", devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

// TestProposalConfirmFanoutFailureRollsBack — when the authoring seam refuses, the card rolls
// back to proposed (retryable) and never posts an executed result.
func TestProposalConfirmFanoutFailureRollsBack(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	life := &fakeProposalLifecycle{}
	life.payload = discussion.ProposalPayload{Action: discussion.ProposalActionCreateTicket, Title: "nope"}
	writer := &fakeWorkItemWriter{err: coord.ErrInvalidWorkItem}
	h := testProposalServer(t, teamID, life, writer, &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, uuid.NewString(), "confirm", devToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !life.failCalled {
		t.Fatal("failed fan-out must roll the lifecycle back to proposed")
	}
	if life.completeCalled {
		t.Fatal("failed fan-out must not post an executed result")
	}
}

// TestProposalConfirmBadMessageID — a non-UUID message id is a 400, not a 500.
func TestProposalConfirmBadMessageID(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	h := testProposalServer(t, teamID, &fakeProposalLifecycle{}, &fakeWorkItemWriter{}, &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, "not-a-uuid", "confirm", devToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

// ---- dismiss -----------------------------------------------------------------

// TestProposalDismiss — dismiss records the decision and does NOTHING else: no create, no
// dispatch, no executed post-back ("no state change" beyond the card's own lifecycle).
func TestProposalDismiss(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	life := &fakeProposalLifecycle{}
	writer := &fakeWorkItemWriter{}
	disp := &fakeDispatcher{}
	h := testProposalServer(t, teamID, life, writer, disp)

	msgID := uuid.NewString()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, msgID, "dismiss", devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !life.dismissCalled {
		t.Fatal("dismiss must reach the lifecycle")
	}
	if life.dismissGot[2].String() != msgID || life.dismissGot[0].String() != projUUID {
		t.Fatalf("dismiss args: %+v", life.dismissGot)
	}
	if writer.createCalled || disp.called || life.completeCalled {
		t.Fatal("dismiss must not execute anything")
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got["status"] != "dismissed" {
		t.Fatalf("body: %v %+v", err, got)
	}
}

// TestProposalDismissAgentForbidden — the dismiss wall matches confirm: humans only.
func TestProposalDismissAgentForbidden(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	life := &fakeProposalLifecycle{}
	h := testProposalServer(t, teamID, life, &fakeWorkItemWriter{}, &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postProposalVerb(projUUID, uuid.NewString(), "dismiss", agentToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
	if life.dismissCalled {
		t.Fatal("agent dismiss must not reach the lifecycle")
	}
}
