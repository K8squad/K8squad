package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

// agentauthor_test.go — the MCP-edge unit lane for the ADR-0024 authoring tools
// (ISI-4734). It pins the capability gate (deny-by-default; a stamped
// work_item.author header allows), the identity requirement (agent run headers),
// the sub-ticket create/update/assign wiring to a fake coord author, the
// create-with-assignee follow-on, and the tool-list advertisement — all without a
// Postgres. The coord-side custody/depth/budget invariants are pinned in
// pkg/coord/agentauthor_unit_test.go.

// fakeAuthor records the coord author calls and returns scripted results, so the
// edge tests assert the gate/marshaling without a DB.
type fakeAuthor struct {
	createCalled bool
	updateCalled bool
	assignCalled bool
	lastIdentity coord.AgentIdentity
	lastCreate   coord.AgentCreateChildInput
	lastUpdateID string
	lastAssignID string
	lastAssignee string
	createRec    coord.WorkItemRecord
	createErr    error
	updateRec    coord.WorkItemRecord
	updateErr    error
	assignRes    coord.WorkItemDispatchResult
	assignErr    error
}

func (f *fakeAuthor) AgentCreateChild(_ context.Context, id coord.AgentIdentity, in coord.AgentCreateChildInput) (coord.WorkItemRecord, error) {
	f.createCalled = true
	f.lastIdentity = id
	f.lastCreate = in
	return f.createRec, f.createErr
}

func (f *fakeAuthor) AgentUpdate(_ context.Context, id coord.AgentIdentity, workItemID string, _ coord.UpdateWorkItemInput) (coord.WorkItemRecord, error) {
	f.updateCalled = true
	f.lastIdentity = id
	f.lastUpdateID = workItemID
	return f.updateRec, f.updateErr
}

func (f *fakeAuthor) AgentAssign(_ context.Context, id coord.AgentIdentity, workItemID, assigneeAgentID string) (coord.WorkItemDispatchResult, error) {
	f.assignCalled = true
	f.lastIdentity = id
	f.lastAssignID = workItemID
	f.lastAssignee = assigneeAgentID
	return f.assignRes, f.assignErr
}

func mountAuthorMCP(author WorkItemAuthor) *http.ServeMux {
	mux := http.NewServeMux()
	NewToolMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil, nil).
		WithWorkItemAuthor(author, NewHeaderCapabilityResolver()).
		Mount(mux)
	return mux
}

// authorHeaders is a fully-authenticated PM agent session WITH the capability.
func authorHeaders() map[string]string {
	return map[string]string{
		"X-Team-Id":            "team-1",
		"X-Principal-Id":       "agent:pm",
		"X-Agent-Id":           "pm",
		"X-Run-Id":             "run-1",
		"X-Agent-Capabilities": "work_item.author",
	}
}

func callTool(t *testing.T, mux *http.ServeMux, headers map[string]string, name, args string) (string, bool, *jsonrpcError) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	resp := rpcCall(t, mux, headers, body)
	if resp.Error != nil {
		return "", false, resp.Error
	}
	text, isErr := resultContentText(t, resp.Result)
	return text, isErr, nil
}

// TestAgentAuthorNoCapabilityDenied — a fully-authenticated agent WITHOUT the
// work_item.author capability is refused, and coord is never touched (ADR-0024 §6 B1).
func TestAgentAuthorNoCapabilityDenied(t *testing.T) {
	author := &fakeAuthor{}
	mux := mountAuthorMCP(author)
	h := authorHeaders()
	delete(h, "X-Agent-Capabilities") // no grant
	text, isErr, rpcErr := callTool(t, mux, h, "work_item_create", `{"parent_id":"p1","title":"x"}`)
	if rpcErr != nil {
		t.Fatalf("protocol error: %+v", rpcErr)
	}
	if !isErr {
		t.Fatalf("want tool error, got success: %s", text)
	}
	if author.createCalled {
		t.Fatal("coord must not be touched for an ungranted agent")
	}
}

// TestAgentAuthorWithCapabilityCreatesChild — capability present ⇒ the create reaches
// coord with the server-stamped identity, and the created record is returned.
func TestAgentAuthorWithCapabilityCreatesChild(t *testing.T) {
	author := &fakeAuthor{createRec: coord.WorkItemRecord{ID: "child-1", State: "backlog"}}
	mux := mountAuthorMCP(author)
	text, isErr, rpcErr := callTool(t, mux, authorHeaders(), "work_item_create", `{"parent_id":"p1","title":"story"}`)
	if rpcErr != nil {
		t.Fatalf("protocol error: %+v", rpcErr)
	}
	if isErr {
		t.Fatalf("unexpected tool error: %s", text)
	}
	if !author.createCalled {
		t.Fatal("coord create must run for a granted agent")
	}
	if author.lastIdentity.AgentID != "pm" || author.lastIdentity.RunID != "run-1" || author.lastIdentity.TeamID != "team-1" {
		t.Fatalf("identity not server-stamped: %+v", author.lastIdentity)
	}
	if author.lastCreate.ParentID != "p1" || author.lastCreate.Title != "story" {
		t.Fatalf("args not forwarded: %+v", author.lastCreate)
	}
	var out workItemCreateResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode result: %v (%s)", err, text)
	}
	if out.WorkItem.ID != "child-1" {
		t.Fatalf("result: %+v", out)
	}
}

// TestAgentAuthorRequiresAgentRun — a human (principal but no agent/run headers) with
// the capability string is still refused: authoring needs a server-authenticated
// agent run, not a spoofable capability alone.
func TestAgentAuthorRequiresAgentRun(t *testing.T) {
	author := &fakeAuthor{}
	mux := mountAuthorMCP(author)
	h := map[string]string{
		"X-Team-Id":            "team-1",
		"X-Principal-Id":       "user:alice",
		"X-Agent-Capabilities": "work_item.author",
	}
	text, isErr, rpcErr := callTool(t, mux, h, "work_item_create", `{"parent_id":"p1","title":"x"}`)
	if rpcErr != nil {
		t.Fatalf("protocol error: %+v", rpcErr)
	}
	if !isErr {
		t.Fatalf("want tool error, got: %s", text)
	}
	if author.createCalled {
		t.Fatal("coord must not run without an agent run identity")
	}
}

// TestAgentAuthorCreateWithAssignee — assignee_agent_id in the same call drives a
// follow-on assign; the created item and the dispatch result both surface.
func TestAgentAuthorCreateWithAssignee(t *testing.T) {
	author := &fakeAuthor{
		createRec: coord.WorkItemRecord{ID: "child-1", State: "backlog"},
		assignRes: coord.WorkItemDispatchResult{WorkItemID: "child-1", ToState: "todo", RequestedAgent: "coder"},
	}
	mux := mountAuthorMCP(author)
	text, isErr, _ := callTool(t, mux, authorHeaders(), "work_item_create", `{"parent_id":"p1","title":"story","assignee_agent_id":"coder"}`)
	if isErr {
		t.Fatalf("unexpected tool error: %s", text)
	}
	if !author.assignCalled || author.lastAssignID != "child-1" || author.lastAssignee != "coder" {
		t.Fatalf("assign not driven: called=%v id=%q who=%q", author.assignCalled, author.lastAssignID, author.lastAssignee)
	}
	var out workItemCreateResult
	_ = json.Unmarshal([]byte(text), &out)
	if out.Assigned == nil || out.Assigned.RequestedAgent != "coder" {
		t.Fatalf("assign result missing: %+v", out)
	}
}

// TestAgentAuthorCreateAssigneeErrorSurfaces — a failed follow-on assign does NOT
// hide the successful create: the child is returned with assignError set.
func TestAgentAuthorCreateAssigneeErrorSurfaces(t *testing.T) {
	author := &fakeAuthor{
		createRec: coord.WorkItemRecord{ID: "child-1"},
		assignErr: coord.ErrAgentAssignUnavailable,
	}
	mux := mountAuthorMCP(author)
	text, isErr, _ := callTool(t, mux, authorHeaders(), "work_item_create", `{"parent_id":"p1","title":"story","assignee_agent_id":"coder"}`)
	if isErr {
		t.Fatalf("create must still succeed: %s", text)
	}
	var out workItemCreateResult
	_ = json.Unmarshal([]byte(text), &out)
	if out.WorkItem.ID != "child-1" || out.AssignError == "" {
		t.Fatalf("want child + assignError, got: %+v", out)
	}
}

// TestAgentAuthorAssignDrivesDispatch — work_item_assign forwards to coord with the
// target agent (ADR-0024 §6 B6).
func TestAgentAuthorAssignDrivesDispatch(t *testing.T) {
	author := &fakeAuthor{assignRes: coord.WorkItemDispatchResult{WorkItemID: "wi-1", ToState: "todo", RequestedAgent: "coder"}}
	mux := mountAuthorMCP(author)
	text, isErr, _ := callTool(t, mux, authorHeaders(), "work_item_assign", `{"id":"wi-1","assignee_agent_id":"coder"}`)
	if isErr {
		t.Fatalf("unexpected tool error: %s", text)
	}
	if !author.assignCalled || author.lastAssignID != "wi-1" || author.lastAssignee != "coder" {
		t.Fatalf("assign not forwarded: %+v", author)
	}
}

// TestAgentAuthorUpdateForwards — work_item_update forwards id + identity to coord.
func TestAgentAuthorUpdateForwards(t *testing.T) {
	author := &fakeAuthor{updateRec: coord.WorkItemRecord{ID: "wi-1", Title: "new"}}
	mux := mountAuthorMCP(author)
	text, isErr, _ := callTool(t, mux, authorHeaders(), "work_item_update", `{"id":"wi-1","title":"new"}`)
	if isErr {
		t.Fatalf("unexpected tool error: %s", text)
	}
	if !author.updateCalled || author.lastUpdateID != "wi-1" {
		t.Fatalf("update not forwarded: %+v", author)
	}
}

// TestAgentAuthorToolsAdvertisedWhenWired — the three tools appear in tools/list once
// the author store is wired; the gate bites at call time, not by hiding the tool.
func TestAgentAuthorToolsAdvertisedWhenWired(t *testing.T) {
	mux := mountAuthorMCP(&fakeAuthor{})
	resp := rpcCall(t, mux, nil, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list error: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var r struct {
		Tools []mcpTool `json:"tools"`
	}
	_ = json.Unmarshal(b, &r)
	names := map[string]bool{}
	for _, tl := range r.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"work_item_create", "work_item_update", "work_item_assign"} {
		if !names[want] {
			t.Fatalf("%q missing from catalog: %v", want, names)
		}
	}
}

// TestAgentAuthorUnmountedWhenNoAuthor — with no author wired the tool is an honest
// "unknown tool" and never advertised (a read-only / DB-less deployment).
func TestAgentAuthorUnmountedWhenNoAuthor(t *testing.T) {
	mux := http.NewServeMux()
	NewToolMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil, nil).Mount(mux)
	text, isErr, rpcErr := callTool(t, mux, authorHeaders(), "work_item_create", `{"parent_id":"p1","title":"x"}`)
	if rpcErr != nil {
		t.Fatalf("protocol error: %+v", rpcErr)
	}
	if !isErr {
		t.Fatalf("want unknown-tool error, got: %s", text)
	}
}

// TestParseCapabilities — comma/space separated, empties dropped, empty ⇒ nil.
func TestParseCapabilities(t *testing.T) {
	if got := parseCapabilities("  "); got != nil {
		t.Fatalf("empty header ⇒ nil, got %v", got)
	}
	got := parseCapabilities("work_item.author, foo bar,")
	want := map[string]bool{"work_item.author": true, "foo": true, "bar": true}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("unexpected capability %q in %v", g, got)
		}
	}
}
