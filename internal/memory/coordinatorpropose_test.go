package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// coordinatorpropose_test.go — the ISI-5282 propose-mode suite for the authoring
// edge. It proves the mode switch at the MCP tool boundary: a coordinator carrying
// the coordinator.propose grant RAISES inert discussion Proposals instead of
// executing coord writes, while an auto-mode coordinator is unchanged, and every
// propose path fails CLOSED (honest refusal, never a silent direct execute).

// --- fakes -----------------------------------------------------------------

type fakeThreadResolver struct {
	projectID string
	teamID    uuid.UUID
	threadID  uuid.UUID
	ok        bool
	err       error
	gotRun    string
	called    bool
}

func (f *fakeThreadResolver) ThreadForDispatchedRun(_ context.Context, runID string) (string, uuid.UUID, uuid.UUID, bool, error) {
	f.called, f.gotRun = true, runID
	return f.projectID, f.teamID, f.threadID, f.ok, f.err
}

type fakeProposalPoster struct {
	called    bool
	projectID string
	teamID    uuid.UUID
	threadID  uuid.UUID
	auth      discussion.AuthorContext
	body      string
	payload   discussion.ProposalPayload
	returnID  uuid.UUID
	err       error
}

func (f *fakeProposalPoster) PostProposal(_ context.Context, projectID string, teamID, threadID uuid.UUID, auth discussion.AuthorContext, body string, payload discussion.ProposalPayload, _ *uuid.UUID) (*discussion.Message, error) {
	f.called = true
	f.projectID, f.teamID, f.threadID, f.auth, f.body, f.payload = projectID, teamID, threadID, auth, body, payload
	if f.err != nil {
		return nil, f.err
	}
	return &discussion.Message{ID: f.returnID, ThreadID: threadID}, nil
}

// proposeSession is a server-authenticated coordinator session in PROPOSE mode:
// it carries BOTH the work_item.author grant (so requireAuthor passes) and the
// coordinator.propose grant (so the edge routes to the proposer).
func proposeSession() mcpSession {
	s := grantedSession()
	s.capabilities = []string{WorkItemAuthorCapability, CoordinatorProposeCapability}
	return s
}

// proposeMCP wires an authoring MCP with a propose channel (resolver + poster) and
// a granting caps resolver, over the given coord fakes.
func proposeMCP(author WorkItemAuthor, dispatcher WorkItemDispatcher, resolver ProposalThreadResolver, poster ProposalPoster) *ToolMCP {
	return newAuthoringMCP(author, dispatcher, &fakeCaps{grant: true}).
		WithCoordinatorPropose(NewCoordinatorProposer(resolver, poster))
}

func okResolver() *fakeThreadResolver {
	return &fakeThreadResolver{projectID: "ns/room", teamID: uuid.New(), threadID: uuid.New(), ok: true}
}

func decodeProposal(t *testing.T, text string) proposalToolResult {
	t.Helper()
	var r proposalToolResult
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatalf("decode propose result %q: %v", text, err)
	}
	return r
}

// --- create path -----------------------------------------------------------

// TestProposeCreateRaisesProposal: a propose-mode work_item_create posts a
// create_ticket proposal into the run's originating thread and NEVER calls coord.
func TestProposeCreateRaisesProposal(t *testing.T) {
	author := &fakeAuthor{}
	resolver := okResolver()
	poster := &fakeProposalPoster{returnID: uuid.New()}
	m := proposeMCP(author, nil, resolver, poster)

	out, rpcErr := m.callWorkItemCreate(context.Background(), proposeSession(),
		createArgs(t, workItemCreateArgs{ParentID: "p", Title: "Add login", Body: "details"}))
	text, isErr := result(t, out, rpcErr)
	if isErr {
		t.Fatalf("propose create should succeed, got error: %q", text)
	}
	if author.createCalled {
		t.Fatal("coord AgentCreateWorkItem was called in propose mode")
	}
	if !poster.called {
		t.Fatal("no proposal was posted")
	}
	if poster.payload.Action != discussion.ProposalActionCreateTicket {
		t.Errorf("want create_ticket action, got %q", poster.payload.Action)
	}
	if poster.payload.Title != "Add login" || poster.payload.Body != "details" {
		t.Errorf("proposal payload did not carry title/body: %+v", poster.payload)
	}
	if poster.projectID != resolver.projectID || poster.threadID != resolver.threadID || poster.teamID != resolver.teamID {
		t.Errorf("proposal not scoped to the resolved thread: %+v", poster)
	}
	if resolver.gotRun != "run-1" {
		t.Errorf("thread resolution keyed off the wrong run: %q", resolver.gotRun)
	}
	// The proposal is agent-authored (an agent may propose, never authorize).
	if poster.auth.AgentID == nil || *poster.auth.AgentID != "john" {
		t.Errorf("proposal author identity wrong: %+v", poster.auth)
	}
	res := decodeProposal(t, text)
	if !res.Proposed || res.Action != discussion.ProposalActionCreateTicket || res.ProposalID != poster.returnID.String() {
		t.Errorf("unexpected tool result: %+v", res)
	}
}

// TestProposeCreateWithAssigneeDefers: a create carrying assignee_agent_id in
// propose mode raises a create_ticket proposal (no assign) and flags the deferral.
func TestProposeCreateWithAssigneeDefers(t *testing.T) {
	resolver := okResolver()
	poster := &fakeProposalPoster{returnID: uuid.New()}
	m := proposeMCP(&fakeAuthor{}, &fakeDispatcher{}, resolver, poster)

	out, rpcErr := m.callWorkItemCreate(context.Background(), proposeSession(),
		createArgs(t, workItemCreateArgs{ParentID: "p", Title: "T", AssigneeAgentID: "ada"}))
	text, isErr := result(t, out, rpcErr)
	if isErr {
		t.Fatalf("unexpected error: %q", text)
	}
	if poster.payload.Action != discussion.ProposalActionCreateTicket {
		t.Errorf("want create_ticket, got %q", poster.payload.Action)
	}
	res := decodeProposal(t, text)
	if res.AssigneeDeferred != "ada" {
		t.Errorf("want deferred assignee ada, got %q", res.AssigneeDeferred)
	}
}

// TestProposeCreateMissingTitleRefused: title is still required for a proposal.
func TestProposeCreateMissingTitleRefused(t *testing.T) {
	resolver := okResolver()
	poster := &fakeProposalPoster{returnID: uuid.New()}
	m := proposeMCP(&fakeAuthor{}, nil, resolver, poster)

	out, rpcErr := m.callWorkItemCreate(context.Background(), proposeSession(),
		createArgs(t, workItemCreateArgs{ParentID: "p"}))
	_, isErr := result(t, out, rpcErr)
	if !isErr {
		t.Fatal("want refusal on missing title")
	}
	if poster.called {
		t.Fatal("a proposal was posted despite a missing title")
	}
}

// --- assign path -----------------------------------------------------------

// TestProposeAssignRaisesProposal: a propose-mode work_item_assign posts an
// assign_agent proposal and NEVER calls the dispatcher.
func TestProposeAssignRaisesProposal(t *testing.T) {
	disp := &fakeDispatcher{}
	resolver := okResolver()
	poster := &fakeProposalPoster{returnID: uuid.New()}
	m := proposeMCP(&fakeAuthor{}, disp, resolver, poster)

	raw, _ := json.Marshal(workItemAssignArgs{ID: "wi-1", AssigneeAgentID: "ada"})
	out, rpcErr := m.callWorkItemAssign(context.Background(), proposeSession(), raw)
	text, isErr := result(t, out, rpcErr)
	if isErr {
		t.Fatalf("propose assign should succeed, got: %q", text)
	}
	if disp.called {
		t.Fatal("dispatcher was called in propose mode")
	}
	if !poster.called || poster.payload.Action != discussion.ProposalActionAssignAgent {
		t.Fatalf("assign_agent proposal not posted: %+v", poster.payload)
	}
	if poster.payload.TicketID != "wi-1" || poster.payload.AssigneeAgentID != "ada" {
		t.Errorf("assign payload wrong: %+v", poster.payload)
	}
	res := decodeProposal(t, text)
	if res.Action != discussion.ProposalActionAssignAgent {
		t.Errorf("want assign_agent result, got %+v", res)
	}
}

// --- auto mode unchanged ---------------------------------------------------

// TestAutoCoordinatorExecutesDirectly: a coordinator WITHOUT the propose grant
// authors directly through coord — propose wiring present but never engaged.
func TestAutoCoordinatorExecutesDirectly(t *testing.T) {
	author := &fakeAuthor{rec: coord.WorkItemRecord{ID: "wi-new"}}
	resolver := okResolver()
	poster := &fakeProposalPoster{returnID: uuid.New()}
	m := proposeMCP(author, nil, resolver, poster)

	// Auto session: work_item.author only, no coordinator.propose.
	out, rpcErr := m.callWorkItemCreate(context.Background(), grantedSession(),
		createArgs(t, workItemCreateArgs{ParentID: "p", Title: "T"}))
	_, isErr := result(t, out, rpcErr)
	if isErr {
		t.Fatal("auto create should execute directly")
	}
	if !author.createCalled {
		t.Fatal("auto mode did not execute the coord create")
	}
	if poster.called {
		t.Fatal("auto mode raised a proposal")
	}
	if resolver.called {
		t.Fatal("auto mode resolved a thread")
	}
}

// --- fail-closed guards ----------------------------------------------------

// TestProposeGatedButUnwiredRefused: a propose-gated call with NO proposer wired
// is refused honestly — never a silent direct execute (which would skip the gate).
func TestProposeGatedButUnwiredRefused(t *testing.T) {
	author := &fakeAuthor{}
	m := newAuthoringMCP(author, nil, &fakeCaps{grant: true}) // no WithCoordinatorPropose

	out, rpcErr := m.callWorkItemCreate(context.Background(), proposeSession(),
		createArgs(t, workItemCreateArgs{ParentID: "p", Title: "T"}))
	text, isErr := result(t, out, rpcErr)
	if !isErr || !strings.Contains(text, "no proposal channel is wired") {
		t.Fatalf("want honest unwired refusal, got isErr=%v text=%q", isErr, text)
	}
	if author.createCalled {
		t.Fatal("coord was executed under a propose gate with no channel")
	}
}

// TestProposeNoOriginatingThreadRefused: a coordinator run that is not a room
// thread-run (resolver ok=false) is refused — no thread to post into.
func TestProposeNoOriginatingThreadRefused(t *testing.T) {
	author := &fakeAuthor{}
	resolver := &fakeThreadResolver{ok: false}
	poster := &fakeProposalPoster{}
	m := proposeMCP(author, nil, resolver, poster)

	out, rpcErr := m.callWorkItemCreate(context.Background(), proposeSession(),
		createArgs(t, workItemCreateArgs{ParentID: "p", Title: "T"}))
	text, isErr := result(t, out, rpcErr)
	if !isErr || !strings.Contains(text, "no originating discussion thread") {
		t.Fatalf("want no-thread refusal, got isErr=%v text=%q", isErr, text)
	}
	if poster.called || author.createCalled {
		t.Fatal("nothing should execute when there is no thread")
	}
}

// TestProposeResolverErrorRefused: a thread-resolution error fails closed.
func TestProposeResolverErrorRefused(t *testing.T) {
	resolver := &fakeThreadResolver{err: errors.New("ledger down")}
	poster := &fakeProposalPoster{}
	m := proposeMCP(&fakeAuthor{}, nil, resolver, poster)

	out, rpcErr := m.callWorkItemCreate(context.Background(), proposeSession(),
		createArgs(t, workItemCreateArgs{ParentID: "p", Title: "T"}))
	text, isErr := result(t, out, rpcErr)
	if !isErr || !strings.Contains(text, "could not resolve") {
		t.Fatalf("want fail-closed resolver refusal, got isErr=%v text=%q", isErr, text)
	}
	if poster.called {
		t.Fatal("a proposal was posted despite a resolver error")
	}
}

// TestProposeStillHonorsCapabilityGate: the propose branch sits AFTER requireAuthor,
// so an ungranted session is denied before propose mode is ever consulted.
func TestProposeStillHonorsCapabilityGate(t *testing.T) {
	resolver := okResolver()
	poster := &fakeProposalPoster{}
	// caps denies work_item.author even though the session claims propose.
	m := newAuthoringMCP(&fakeAuthor{}, nil, &fakeCaps{grant: false}).
		WithCoordinatorPropose(NewCoordinatorProposer(resolver, poster))

	out, rpcErr := m.callWorkItemCreate(context.Background(), proposeSession(),
		createArgs(t, workItemCreateArgs{ParentID: "p", Title: "T"}))
	text, isErr := result(t, out, rpcErr)
	if !isErr || !strings.Contains(text, "capability denied") {
		t.Fatalf("want capability-denied before propose, got isErr=%v text=%q", isErr, text)
	}
	if resolver.called || poster.called {
		t.Fatal("propose mode engaged despite a denied capability")
	}
}
