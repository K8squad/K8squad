package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

// agentauthor_test.go — the ADR-0024 §6B MCP capability suite for the agent
// work-item authoring edge (ISI-4741). It exercises the EDGE behavior the slice
// adds — the capability gate, the header→coord identity marshaling, and the honest
// error/assign surfacing — with fakes standing in for the coord backends (the coord
// custody/depth/budget invariants themselves are proven in pkg/coord's own suites).

// TestWorkItemCreateToolInstructsAuthoring is the ISI-4872 (ADR-0024a S6)
// instruction regression: the work_item_create tool description — the one
// instruction surface guaranteed to render whenever the tool is mounted (and,
// by S2 deny-by-default, mounted ONLY for a granted decomposing agent) — must
// explicitly tell the agent to create sub-tickets via the tool and NOT treat a
// markdown story file as the deliverable. This is the concrete fix for the
// ISI-4855 symptom where quill wrote docs/03-stories.md and created zero tickets.
func TestWorkItemCreateToolInstructsAuthoring(t *testing.T) {
	desc := workItemCreateTool.Description
	for _, want := range []string{"markdown", "not a ticket"} {
		if !strings.Contains(strings.ToLower(desc), strings.ToLower(want)) {
			t.Errorf("work_item_create description must warn against %q; got: %q", want, desc)
		}
	}
	if !strings.Contains(strings.ToLower(desc), "create each") && !strings.Contains(strings.ToLower(desc), "create every") {
		t.Errorf("work_item_create description must direct the agent to create each sub-ticket via the tool; got: %q", desc)
	}
}

// --- fakes -----------------------------------------------------------------

type fakeAuthor struct {
	createIn     coord.AgentCreateWorkItemInput
	createCalled bool
	updateID     string
	updateIn     coord.AgentUpdateWorkItemInput
	rec          coord.WorkItemRecord
	createErr    error
	updateErr    error
}

func (f *fakeAuthor) AgentCreateWorkItem(_ context.Context, in coord.AgentCreateWorkItemInput) (coord.WorkItemRecord, error) {
	f.createCalled, f.createIn = true, in
	if f.createErr != nil {
		return coord.WorkItemRecord{}, f.createErr
	}
	return f.rec, nil
}

func (f *fakeAuthor) AgentUpdateWorkItem(_ context.Context, id string, in coord.AgentUpdateWorkItemInput) (coord.WorkItemRecord, error) {
	f.updateID, f.updateIn = id, in
	if f.updateErr != nil {
		return coord.WorkItemRecord{}, f.updateErr
	}
	return f.rec, nil
}

type fakeDispatcher struct {
	in     coord.AgentRequestDispatchInput
	res    coord.WorkItemDispatchResult
	err    error
	called bool
}

func (f *fakeDispatcher) AgentRequestDispatch(_ context.Context, in coord.AgentRequestDispatchInput) (coord.WorkItemDispatchResult, error) {
	f.called, f.in = true, in
	if f.err != nil {
		return coord.WorkItemDispatchResult{}, f.err
	}
	return f.res, nil
}

type fakeCaps struct {
	grant  bool
	err    error
	called bool
	saw    AgentSession
}

func (f *fakeCaps) HasWorkItemAuthor(_ context.Context, sess AgentSession) (bool, error) {
	f.called, f.saw = true, sess
	return f.grant, f.err
}

// --- helpers ---------------------------------------------------------------

func strptr(s string) *string { return &s }

// grantedSession is a fully server-authenticated agent session (the headers the BFF
// stamps). Individual tests override fields to exercise the missing-header guards.
func grantedSession() mcpSession {
	return mcpSession{
		team:         "team-1",
		principal:    "agent:john",
		agentID:      strptr("john"),
		runID:        strptr("run-1"),
		capabilities: []string{WorkItemAuthorCapability},
	}
}

// result decodes a handler's (any, *jsonrpcError) into (text, isError). A non-nil
// jsonrpcError is a protocol-level failure (never expected for these tools).
func result(t *testing.T, out any, rpcErr *jsonrpcError) (string, bool) {
	t.Helper()
	if rpcErr != nil {
		t.Fatalf("unexpected protocol error: %+v", rpcErr)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var parsed struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(parsed.Content) == 0 {
		t.Fatalf("result had no content: %s", b)
	}
	return parsed.Content[0].Text, parsed.IsError
}

func newAuthoringMCP(author WorkItemAuthor, dispatcher WorkItemDispatcher, caps CapabilityResolver) *ToolMCP {
	return NewToolMCP(nil, nil, nil).WithWorkItemAuthor(author, dispatcher, caps)
}

func createArgs(t *testing.T, a workItemCreateArgs) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

// --- capability gate -------------------------------------------------------

// TestAgentAuthorNoCapabilityDenied — a session WITHOUT work_item.author is refused
// with an honest capability-denied error, and coord is never touched.
func TestAgentAuthorNoCapabilityDenied(t *testing.T) {
	author := &fakeAuthor{}
	caps := &fakeCaps{grant: false}
	m := newAuthoringMCP(author, nil, caps)

	sess := grantedSession()
	sess.capabilities = nil // no grant
	out, rpcErr := m.callWorkItemCreate(context.Background(), sess, createArgs(t, workItemCreateArgs{ParentID: "p", Title: "t"}))
	text, isErr := result(t, out, rpcErr)
	if !isErr || !strings.Contains(text, "capability denied") {
		t.Fatalf("want capability-denied, got isErr=%v text=%q", isErr, text)
	}
	if author.createCalled {
		t.Fatal("coord AgentCreateWorkItem was called despite a denied capability")
	}
}

// TestAgentAuthorNilResolverDenies — with no capability resolver wired at all, every
// call is denied (deny-by-default), coord untouched.
func TestAgentAuthorNilResolverDenies(t *testing.T) {
	author := &fakeAuthor{}
	m := newAuthoringMCP(author, nil, nil) // nil caps
	out, rpcErr := m.callWorkItemCreate(context.Background(), grantedSession(), createArgs(t, workItemCreateArgs{ParentID: "p", Title: "t"}))
	text, isErr := result(t, out, rpcErr)
	if !isErr || !strings.Contains(text, "deny-by-default") {
		t.Fatalf("want deny-by-default refusal, got isErr=%v text=%q", isErr, text)
	}
}

// TestAgentAuthorResolverErrorFailsClosed — a resolver failure refuses the call
// (fail-closed), never allows it.
func TestAgentAuthorResolverErrorFailsClosed(t *testing.T) {
	author := &fakeAuthor{}
	caps := &fakeCaps{err: errors.New("role source unreachable")}
	m := newAuthoringMCP(author, nil, caps)
	out, rpcErr := m.callWorkItemCreate(context.Background(), grantedSession(), createArgs(t, workItemCreateArgs{ParentID: "p", Title: "t"}))
	text, isErr := result(t, out, rpcErr)
	if !isErr || !strings.Contains(text, "capability check unavailable") {
		t.Fatalf("want fail-closed refusal, got isErr=%v text=%q", isErr, text)
	}
	if author.createCalled {
		t.Fatal("coord was called despite a resolver error")
	}
}

// TestAgentAuthorRequiresPrincipalAndRun — the identity guards refuse before the
// capability check when the server-authenticated principal / agent / run is absent.
func TestAgentAuthorRequiresPrincipalAndRun(t *testing.T) {
	cases := []struct {
		name string
		mut  func(s *mcpSession)
		want string
	}{
		{"no principal", func(s *mcpSession) { s.principal = "" }, "X-Principal-Id required"},
		{"no agent", func(s *mcpSession) { s.agentID = nil }, "server-authenticated agent run"},
		{"no run", func(s *mcpSession) { s.runID = nil }, "server-authenticated agent run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := &fakeCaps{grant: true}
			m := newAuthoringMCP(&fakeAuthor{}, nil, caps)
			sess := grantedSession()
			tc.mut(&sess)
			out, rpcErr := m.callWorkItemCreate(context.Background(), sess, createArgs(t, workItemCreateArgs{ParentID: "p", Title: "t"}))
			text, isErr := result(t, out, rpcErr)
			if !isErr || !strings.Contains(text, tc.want) {
				t.Fatalf("want %q, got isErr=%v text=%q", tc.want, isErr, text)
			}
			if caps.called {
				t.Fatal("capability resolver consulted despite a missing-identity guard")
			}
		})
	}
}

// TestAgentAuthorRequiresTeam (F2, ISI-4746) — a fully-authenticated agent run WITH
// the capability but NO X-Team-Id is refused before the capability check: authoring
// needs the server-authenticated team scope (it is threaded into the PM→implementer
// assign's target-∈-Team guard), never an empty tenancy.
func TestAgentAuthorRequiresTeam(t *testing.T) {
	caps := &fakeCaps{grant: true}
	author := &fakeAuthor{}
	m := newAuthoringMCP(author, nil, caps)
	sess := grantedSession()
	sess.team = "" // no team scope
	out, rpcErr := m.callWorkItemCreate(context.Background(), sess, createArgs(t, workItemCreateArgs{ParentID: "p", Title: "t"}))
	text, isErr := result(t, out, rpcErr)
	if !isErr || !strings.Contains(text, "team scope") {
		t.Fatalf("want team-scope refusal, got isErr=%v text=%q", isErr, text)
	}
	if caps.called {
		t.Fatal("capability resolver consulted despite a missing team scope")
	}
	if author.createCalled {
		t.Fatal("coord was called without a team scope")
	}
}

// --- create ----------------------------------------------------------------

// TestAgentAuthorWithCapabilityCreatesChild — the happy path: identity is folded
// from the session headers into the coord input (never a tool arg), and the created
// record is returned.
func TestAgentAuthorWithCapabilityCreatesChild(t *testing.T) {
	author := &fakeAuthor{rec: coord.WorkItemRecord{ID: "child-1", Title: "child", State: "backlog"}}
	caps := &fakeCaps{grant: true}
	m := newAuthoringMCP(author, nil, caps)

	out, rpcErr := m.callWorkItemCreate(context.Background(), grantedSession(),
		createArgs(t, workItemCreateArgs{ParentID: "epic-1", Title: "child", Priority: "high"}))
	text, isErr := result(t, out, rpcErr)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	// Identity came from the SESSION, not the arguments.
	if author.createIn.Principal != "agent:john" || author.createIn.AgentName != "john" || author.createIn.RunID != "run-1" {
		t.Fatalf("identity not folded from session headers: %+v", author.createIn)
	}
	if author.createIn.ParentID != "epic-1" || author.createIn.Title != "child" || author.createIn.Priority != "high" {
		t.Fatalf("body args not forwarded: %+v", author.createIn)
	}
	if !strings.Contains(text, `"child-1"`) {
		t.Fatalf("created record not returned: %s", text)
	}
	// The capability resolver saw the server-authenticated session.
	if !caps.called || caps.saw.Principal != "agent:john" || caps.saw.AgentID != "john" {
		t.Fatalf("resolver did not see the session identity: %+v", caps.saw)
	}
}

// TestAgentAuthorCreateSurfacesCoordGuards — the coord custody/fan-out sentinels are
// surfaced verbatim as honest tool errors (root-denied, not-in-custody, depth,
// budget). The edge does not swallow or rename them.
func TestAgentAuthorCreateSurfacesCoordGuards(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"root denied", coord.ErrAgentAuthorRootDenied},
		{"not in custody", coord.ErrAgentAuthorNotInCustody},
		{"depth cap", coord.ErrAgentAuthorDepthExceeded},
		{"run budget", coord.ErrAgentAuthorRunBudgetExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			author := &fakeAuthor{createErr: tc.err}
			m := newAuthoringMCP(author, nil, &fakeCaps{grant: true})
			out, rpcErr := m.callWorkItemCreate(context.Background(), grantedSession(),
				createArgs(t, workItemCreateArgs{ParentID: "p", Title: "t"}))
			text, isErr := result(t, out, rpcErr)
			if !isErr || !strings.Contains(text, tc.err.Error()) {
				t.Fatalf("want surfaced %q, got isErr=%v text=%q", tc.err, isErr, text)
			}
		})
	}
}

// TestAgentAuthorCreateWithAssigneeDrivesDispatch — assignee_agent_id in the create
// call fires a follow-on AgentRequestDispatch against the created id, and the
// dispatch outcome rides back in the result.
func TestAgentAuthorCreateWithAssigneeDrivesDispatch(t *testing.T) {
	author := &fakeAuthor{rec: coord.WorkItemRecord{ID: "child-9", Title: "child"}}
	disp := &fakeDispatcher{res: coord.WorkItemDispatchResult{WorkItemID: "child-9", ToState: "todo", RequestedAgent: "amelia"}}
	m := newAuthoringMCP(author, disp, &fakeCaps{grant: true})

	out, rpcErr := m.callWorkItemCreate(context.Background(), grantedSession(),
		createArgs(t, workItemCreateArgs{ParentID: "epic-1", Title: "child", AssigneeAgentID: "amelia"}))
	text, isErr := result(t, out, rpcErr)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !disp.called || disp.in.WorkItemID != "child-9" || disp.in.AssigneeAgentID != "amelia" {
		t.Fatalf("dispatch not driven for the created child: called=%v in=%+v", disp.called, disp.in)
	}
	if disp.in.Principal != "agent:john" || disp.in.AgentName != "john" || disp.in.TeamID != "team-1" {
		t.Fatalf("dispatch identity not folded from session: %+v", disp.in)
	}
	if !strings.Contains(text, `"assigned"`) || !strings.Contains(text, "todo") {
		t.Fatalf("assign outcome not surfaced: %s", text)
	}
}

// TestAgentAuthorCreateAssigneeErrorSurfaces — a failed follow-on assign does NOT
// hide the successful create: the child is returned with an assignError.
func TestAgentAuthorCreateAssigneeErrorSurfaces(t *testing.T) {
	author := &fakeAuthor{rec: coord.WorkItemRecord{ID: "child-9"}}
	disp := &fakeDispatcher{err: coord.ErrAgentNotInTeam}
	m := newAuthoringMCP(author, disp, &fakeCaps{grant: true})

	out, rpcErr := m.callWorkItemCreate(context.Background(), grantedSession(),
		createArgs(t, workItemCreateArgs{ParentID: "epic-1", Title: "child", AssigneeAgentID: "outsider"}))
	text, isErr := result(t, out, rpcErr)
	if isErr {
		t.Fatalf("create must still succeed when the follow-on assign fails: %s", text)
	}
	if !strings.Contains(text, `"child-9"`) || !strings.Contains(text, "assignError") {
		t.Fatalf("want created child + assignError, got %s", text)
	}
}

// TestAgentAuthorCreateAssigneeUnavailableWithoutDispatcher — with no dispatch
// backend wired, a create+assign still creates the child and reports the assign as
// unavailable (never a silent drop).
func TestAgentAuthorCreateAssigneeUnavailableWithoutDispatcher(t *testing.T) {
	author := &fakeAuthor{rec: coord.WorkItemRecord{ID: "child-9"}}
	m := newAuthoringMCP(author, nil, &fakeCaps{grant: true}) // nil dispatcher
	out, rpcErr := m.callWorkItemCreate(context.Background(), grantedSession(),
		createArgs(t, workItemCreateArgs{ParentID: "epic-1", Title: "child", AssigneeAgentID: "amelia"}))
	text, isErr := result(t, out, rpcErr)
	if isErr {
		t.Fatalf("create must succeed even without a dispatch backend: %s", text)
	}
	if !strings.Contains(text, `"child-9"`) || !strings.Contains(text, "unavailable") {
		t.Fatalf("want created child + unavailable assign note, got %s", text)
	}
}

// --- update ----------------------------------------------------------------

// TestAgentAuthorUpdateForwards — the update tool folds identity from the session
// and forwards the field pointers; a missing id is refused before coord.
func TestAgentAuthorUpdateForwards(t *testing.T) {
	author := &fakeAuthor{rec: coord.WorkItemRecord{ID: "item-2", Title: "new"}}
	m := newAuthoringMCP(author, nil, &fakeCaps{grant: true})

	raw, _ := json.Marshal(workItemUpdateArgs{ID: "item-2", Title: strptr("new")})
	out, rpcErr := m.callWorkItemUpdate(context.Background(), grantedSession(), raw)
	text, isErr := result(t, out, rpcErr)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if author.updateID != "item-2" || author.updateIn.Principal != "agent:john" || author.updateIn.AgentName != "john" || author.updateIn.RunID != "run-1" {
		t.Fatalf("update did not fold identity / id: id=%q in=%+v", author.updateID, author.updateIn)
	}

	// missing id → refused before coord.
	author2 := &fakeAuthor{}
	m2 := newAuthoringMCP(author2, nil, &fakeCaps{grant: true})
	raw2, _ := json.Marshal(workItemUpdateArgs{Title: strptr("x")})
	out2, rpcErr2 := m2.callWorkItemUpdate(context.Background(), grantedSession(), raw2)
	text2, isErr2 := result(t, out2, rpcErr2)
	if !isErr2 || !strings.Contains(text2, "id required") {
		t.Fatalf("want id-required refusal, got isErr=%v text=%q", isErr2, text2)
	}
	if author2.updateID != "" {
		t.Fatal("coord update called despite missing id")
	}
}

// --- assign ----------------------------------------------------------------

// TestAgentAuthorAssignDrivesDispatch — the assign tool drives AgentRequestDispatch
// with the session identity; a target-not-in-team refusal from coord surfaces
// honestly.
func TestAgentAuthorAssignDrivesDispatch(t *testing.T) {
	disp := &fakeDispatcher{res: coord.WorkItemDispatchResult{WorkItemID: "item-3", ToState: "todo", RequestedAgent: "amelia"}}
	m := newAuthoringMCP(&fakeAuthor{}, disp, &fakeCaps{grant: true})

	raw, _ := json.Marshal(workItemAssignArgs{ID: "item-3", AssigneeAgentID: "amelia"})
	out, rpcErr := m.callWorkItemAssign(context.Background(), grantedSession(), raw)
	text, isErr := result(t, out, rpcErr)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !disp.called || disp.in.WorkItemID != "item-3" || disp.in.AssigneeAgentID != "amelia" || disp.in.TeamID != "team-1" {
		t.Fatalf("dispatch not driven with session scope: %+v", disp.in)
	}

	// target-not-in-team → surfaced.
	disp2 := &fakeDispatcher{err: coord.ErrAgentNotInTeam}
	m2 := newAuthoringMCP(&fakeAuthor{}, disp2, &fakeCaps{grant: true})
	out2, rpcErr2 := m2.callWorkItemAssign(context.Background(), grantedSession(), raw)
	text2, isErr2 := result(t, out2, rpcErr2)
	if !isErr2 || !strings.Contains(text2, coord.ErrAgentNotInTeam.Error()) {
		t.Fatalf("want target-not-in-team surfaced, got isErr=%v text=%q", isErr2, text2)
	}
}

// TestAgentAuthorAssignUnavailableWithoutDispatcher — assign with no dispatch
// backend is refused honestly (capability held, but the deployment can't dispatch).
func TestAgentAuthorAssignUnavailableWithoutDispatcher(t *testing.T) {
	m := newAuthoringMCP(&fakeAuthor{}, nil, &fakeCaps{grant: true}) // nil dispatcher
	raw, _ := json.Marshal(workItemAssignArgs{ID: "item-3", AssigneeAgentID: "amelia"})
	out, rpcErr := m.callWorkItemAssign(context.Background(), grantedSession(), raw)
	text, isErr := result(t, out, rpcErr)
	if !isErr || !strings.Contains(text, "unavailable") {
		t.Fatalf("want assign-unavailable, got isErr=%v text=%q", isErr, text)
	}
}

// --- advertisement + parsing ----------------------------------------------

// TestAgentAuthorToolsAdvertisedWhenWired — the three tools appear in tools/list iff
// an author store is wired; a read-only deployment leaves them out.
func TestAgentAuthorToolsAdvertisedWhenWired(t *testing.T) {
	names := func(m *ToolMCP) map[string]bool {
		b, _ := json.Marshal(m.toolsList())
		var parsed struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		_ = json.Unmarshal(b, &parsed)
		out := map[string]bool{}
		for _, tl := range parsed.Tools {
			out[tl.Name] = true
		}
		return out
	}

	wired := names(newAuthoringMCP(&fakeAuthor{}, nil, &fakeCaps{grant: true}))
	for _, n := range []string{"work_item_create", "work_item_update", "work_item_assign"} {
		if !wired[n] {
			t.Fatalf("tool %q not advertised when author wired", n)
		}
	}

	bare := names(NewToolMCP(nil, nil, nil))
	for _, n := range []string{"work_item_create", "work_item_update", "work_item_assign"} {
		if bare[n] {
			t.Fatalf("tool %q advertised without an author store", n)
		}
	}
}

// TestAgentAuthorUnmountedRejectsCall — with no author store, tools/call refuses the
// verb as an unknown tool (unmounted), never a nil-deref.
func TestAgentAuthorUnmountedRejectsCall(t *testing.T) {
	m := NewToolMCP(nil, nil, nil)
	params, _ := json.Marshal(map[string]any{"name": "work_item_create", "arguments": map[string]any{"parent_id": "p", "title": "t"}})
	sess := grantedSession()
	out, rpcErr := m.toolsCall(context.Background(), sess, params)
	text, isErr := result(t, out, rpcErr)
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("want unknown-tool refusal when unmounted, got isErr=%v text=%q", isErr, text)
	}
}

func TestParseCapabilities(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"work_item.author", []string{"work_item.author"}},
		{"a, b ,c", []string{"a", "b", "c"}},
		{" a\tb  c ", []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		got := parseCapabilities(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("parseCapabilities(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("parseCapabilities(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestHeaderCapabilityResolver(t *testing.T) {
	r := NewHeaderCapabilityResolver()
	ok, err := r.HasWorkItemAuthor(context.Background(), AgentSession{Capabilities: []string{"other", WorkItemAuthorCapability}})
	if err != nil || !ok {
		t.Fatalf("want granted, got ok=%v err=%v", ok, err)
	}
	ok, err = r.HasWorkItemAuthor(context.Background(), AgentSession{Capabilities: []string{"other"}})
	if err != nil || ok {
		t.Fatalf("want denied (no grant), got ok=%v err=%v", ok, err)
	}
	ok, _ = r.HasWorkItemAuthor(context.Background(), AgentSession{})
	if ok {
		t.Fatal("empty capability set must deny (deny-by-default)")
	}
}
