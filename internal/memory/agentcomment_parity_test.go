package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

// agentcomment_parity_test.go — the ISI-7220 (epic ISI-7075, workstream B3)
// MCP capability suite for the work_item_comment tool: cross-agent reference
// parity on the ticket-run surface. Mirrors the discipline of
// agentauthor_test.go (edge behavior with fakes; the coord custody/tenancy
// invariants are proven in pkg/coord's own suites) and references_parity_test.go
// (the drift class closed is "discussion_post persists refs/mentions, the board
// comment tool silently doesn't").

// commentItemID / commentRefID are fixed probe UUIDs (same discipline as
// references_parity_test.go's refInProject).
const (
	commentItemID = "11111111-1111-1111-1111-111111111111"
	commentRefID  = "22222222-2222-2222-2222-222222222222"
)

// commentArgs builds one tools/call argument object for the comment tool.
func commentArgs(id, body string, extra string) json.RawMessage {
	arg := fmt.Sprintf(`{"id":%q,"body":%q%s}`, id, body, extra)
	return json.RawMessage(arg)
}

// --- fakes -----------------------------------------------------------------

// fakeCommentAuthor captures the ONE call this suite exercises.
type fakeCommentAuthor struct {
	in     coord.AgentCommentInput
	called bool
	rec    coord.TaskComment
	err    error
}

func (f *fakeCommentAuthor) AgentCreateWorkItem(_ context.Context, in coord.AgentCreateWorkItemInput) (coord.WorkItemRecord, error) {
	return coord.WorkItemRecord{}, nil
}

func (f *fakeCommentAuthor) AgentUpdateWorkItem(_ context.Context, id string, in coord.AgentUpdateWorkItemInput) (coord.WorkItemRecord, error) {
	return coord.WorkItemRecord{}, nil
}

func (f *fakeCommentAuthor) RunHeldWorkItem(_ context.Context, runID string) (string, error) {
	return "", nil
}

func (f *fakeCommentAuthor) AgentAppendComment(_ context.Context, in coord.AgentCommentInput) (coord.TaskComment, error) {
	f.called, f.in = true, in
	if f.err != nil {
		return coord.TaskComment{}, f.err
	}
	return f.rec, nil
}

// fakeRoster is the coord.TeamAgentResolver seam: a fixed Team composition.
type fakeRoster struct {
	agents []string
	err    error
	called bool
	team   string
}

func (f *fakeRoster) TeamAgents(_ context.Context, teamUID string) ([]string, error) {
	f.called, f.team = true, teamUID
	return f.agents, f.err
}

// commentServer builds a ToolMCP with the authoring lane wired for comments.
func commentServer(author WorkItemAuthor, roster coord.TeamAgentResolver, grant bool) *ToolMCP {
	m := NewToolMCP(nil, nil, nil).WithWorkItemAuthor(author, nil, &fakeCaps{grant: grant})
	if roster != nil {
		m = m.WithTeamRoster(roster)
	}
	return m
}

// commentCall runs one tools/call through the handler with a granted session.
func commentCall(t *testing.T, m *ToolMCP, args json.RawMessage) (string, bool) {
	t.Helper()
	out, rpcErr := m.callWorkItemComment(context.Background(), grantedSession(), args)
	return result(t, out, rpcErr)
}

// --- tests -----------------------------------------------------------------

// TestWorkItemCommentToolInManifest is the drift guard: the compiled-in
// authoring manifest must carry the comment verb (the operator seeds
// status.observedTools from AuthoringToolNames — a missing name would leave
// the tool unmounted cluster-wide with no compile error).
func TestWorkItemCommentToolInManifest(t *testing.T) {
	found := false
	for _, name := range AuthoringToolNames {
		if name == WorkItemCommentToolName {
			found = true
		}
	}
	if !found {
		t.Fatalf("AuthoringToolNames = %v, want %s present", AuthoringToolNames, WorkItemCommentToolName)
	}
	// And the catalog advertises it whenever the author store is wired, so a
	// client's tools/list matches what tools/call serves.
	list := commentServer(&fakeCommentAuthor{}, nil, true).toolsList()
	tools, _ := list.(map[string]any)["tools"].([]mcpTool)
	advertised := false
	for _, tl := range tools {
		if tl.Name == WorkItemCommentToolName {
			advertised = true
		}
	}
	if !advertised {
		t.Fatalf("tools/list missing %s when author wired", WorkItemCommentToolName)
	}
}

// TestWorkItemCommentToolInstructsNoDispatch pins the B3 semantic contract in
// the one instruction surface guaranteed to render when the tool is mounted:
// the description must teach that mentions/references are LINKS and that the
// make-an-agent-act verb is work_item_assign — the FR-B3 no-chat contract,
// taught where the model reads it (the ISI-4855 lesson: behavior follows the
// tool description).
func TestWorkItemCommentToolInstructsNoDispatch(t *testing.T) {
	desc := strings.ToLower(workItemCommentTool.Description)
	for _, want := range []string{"work_item_assign", "never a dispatch"} {
		if !strings.Contains(desc, want) {
			t.Errorf("work_item_comment description must teach %q; got: %q", want, desc)
		}
	}
}

// TestWorkItemCommentPersistsRefsAndMentions is the B3 parity AC over the MCP
// edge: a comment posted with references + mentions reaches coord with BOTH
// primitives and the server-authenticated identity (never arguments) — the
// board peer of TestToolTicketRefPersistsMCP.
func TestWorkItemCommentPersistsRefsAndMentions(t *testing.T) {
	author := &fakeCommentAuthor{rec: coord.TaskComment{Author: "agent:john", Body: "see also"}}
	m := commentServer(author, &fakeRoster{agents: []string{"john", "kay"}}, true)

	extra := fmt.Sprintf(`,"references":[{"workItemId":%q,"title":"stale"}],"mentions":["kay","ghost"]`, commentRefID)
	body, isErr := commentCall(t, m, commentArgs(commentItemID, "handoff note", extra))
	if isErr {
		t.Fatalf("work_item_comment tool error: %s", body)
	}
	if !author.called {
		t.Fatal("coord.AgentAppendComment not called")
	}
	in := author.in
	if in.WorkItemID != commentItemID || in.Body != "handoff note" {
		t.Fatalf("captured id/body = %q/%q", in.WorkItemID, in.Body)
	}
	// Identity rides the session headers, never the arguments.
	if in.Principal != "agent:john" || in.AgentName != "john" || in.RunID != "run-1" || in.TeamID != "team-1" {
		t.Fatalf("captured identity = %+v, want the granted session identity", in)
	}
	// References pass through verbatim (coord resolves them in-txn against the
	// ticket's project — the same pipeline the human path rides).
	if len(in.References) != 1 || in.References[0].WorkItemID != commentRefID {
		t.Fatalf("captured references = %+v", in.References)
	}
	// Mentions are filtered to the Team roster BEFORE the coord call: "ghost"
	// is dropped, "kay" survives — a durable reference can never be spoofed.
	if len(in.Mentions) != 1 || in.Mentions[0] != "kay" {
		t.Fatalf("captured mentions = %v, want [kay] (ghost dropped by roster)", in.Mentions)
	}
	if !strings.Contains(body, "see also") {
		t.Fatalf("tool result missing the persisted comment: %s", body)
	}
}

// TestWorkItemCommentRosterScoping proves the roster check rides the
// server-authenticated team scope (the caller's Team, never an argument).
func TestWorkItemCommentRosterScoping(t *testing.T) {
	author := &fakeCommentAuthor{rec: coord.TaskComment{Body: "x"}}
	roster := &fakeRoster{agents: []string{"kay"}}
	m := commentServer(author, roster, true)
	if _, isErr := commentCall(t, m, commentArgs(commentItemID, "note", `,"mentions":["kay"]`)); isErr {
		t.Fatal("unexpected tool error")
	}
	if !roster.called || roster.team != "team-1" {
		t.Fatalf("roster scope = {called %v, team %q}, want the session team", roster.called, roster.team)
	}
}

// TestWorkItemCommentNilRosterDegrades proves the best-effort degrade the
// discussion refs path shares (TestToolTicketRefNilResolverDrops): with no
// roster wired, mentions are dropped and the comment still lands — true
// parity, no regression, never a refusal.
func TestWorkItemCommentNilRosterDegrades(t *testing.T) {
	author := &fakeCommentAuthor{rec: coord.TaskComment{Body: "plain"}}
	m := commentServer(author, nil, true)
	if _, isErr := commentCall(t, m, commentArgs(commentItemID, "note", `,"mentions":["kay"]`)); isErr {
		t.Fatal("unexpected tool error")
	}
	if !author.called {
		t.Fatal("comment must still land mention-free")
	}
	if len(author.in.Mentions) != 0 {
		t.Fatalf("mentions = %v, want empty (nil roster degrades)", author.in.Mentions)
	}
}

// TestWorkItemCommentRosterErrorFailsClosed: a resolver failure refuses the
// call (fail-closed), never persists an unvalidated mention.
func TestWorkItemCommentRosterErrorFailsClosed(t *testing.T) {
	author := &fakeCommentAuthor{}
	m := commentServer(author, &fakeRoster{err: context.DeadlineExceeded}, true)
	body, isErr := commentCall(t, m, commentArgs(commentItemID, "note", `,"mentions":["kay"]`))
	if !isErr {
		t.Fatalf("roster failure must fail closed; got result %s", body)
	}
	if author.called {
		t.Fatal("coord must not be reached on a roster failure")
	}
}

// TestWorkItemCommentGateAndCustody: the capability gate refuses an ungranted
// session before coord is touched, and a coord custody refusal (ADR-0024
// scope) surfaces as an honest tool error the model can act on.
func TestWorkItemCommentGateAndCustody(t *testing.T) {
	author := &fakeCommentAuthor{}
	denied := commentServer(author, nil, false)
	body, isErr := commentCall(t, denied, commentArgs(commentItemID, "note", ""))
	if !isErr || !strings.Contains(body, "capability denied") {
		t.Fatalf("ungranted session = {err %v, body %q}, want capability denied", isErr, body)
	}
	if author.called {
		t.Fatal("coord must not be reached when the gate denies")
	}

	custody := commentServer(&fakeCommentAuthor{err: coord.ErrAgentAuthorNotInCustody}, nil, true)
	body, isErr = commentCall(t, custody, commentArgs(commentItemID, "note", ""))
	if !isErr || !strings.Contains(body, "custody") {
		t.Fatalf("custody refusal = {err %v, body %q}, want honest custody error", isErr, body)
	}
}

// TestWorkItemCommentValidation: id and body are required arguments.
func TestWorkItemCommentValidation(t *testing.T) {
	m := commentServer(&fakeCommentAuthor{}, nil, true)
	if body, isErr := commentCall(t, m, commentArgs("", "no id", "")); !isErr || !strings.Contains(body, "id and body required") {
		t.Fatalf("missing id = {err %v, body %q}, want id-and-body-required", isErr, body)
	}
	if body, isErr := commentCall(t, m, commentArgs(commentItemID, "", "")); !isErr {
		t.Fatalf("missing body = {err %v, body %q}, want error", isErr, body)
	}
}
