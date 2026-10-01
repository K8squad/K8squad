package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/search"
)

// fakeWorkItemSearcher is a canned pkg/search.Searcher that records the Query it was handed so a test can
// assert the RBAC scope the MCP edge binds (ISI-5276). It returns results verbatim, or err when set.
type fakeWorkItemSearcher struct {
	got     search.Query
	results []search.Result
	err     error
}

func (f *fakeWorkItemSearcher) Search(_ context.Context, q search.Query) ([]search.Result, error) {
	f.got = q
	return f.results, f.err
}

func mountMCPWithSearcher(finder search.Searcher) (*http.ServeMux, *ToolMCP) {
	mux := http.NewServeMux()
	m := NewToolMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil, nil).WithWorkItemSearch(finder)
	m.Mount(mux)
	return mux, m
}

// TestMCP_ToolsList_WorkItemSearchGatedOnSearcher asserts work_item_search is advertised exactly when a
// Searcher is wired — a DB-less deployment leaves it out, matching discussion_post's gate.
func TestMCP_ToolsList_WorkItemSearchGatedOnSearcher(t *testing.T) {
	// Present when a searcher is wired.
	mux, _ := mountMCPWithSearcher(&fakeWorkItemSearcher{})
	resp := rpcCall(t, mux, nil, `{"jsonrpc":"2.0","id":80,"method":"tools/list"}`)
	if !listHasTool(t, resp, "work_item_search") {
		t.Fatalf("work_item_search must be advertised when a searcher is wired")
	}
	// Absent when no searcher is wired (read-only deployment).
	mux2 := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	resp2 := rpcCall(t, mux2, nil, `{"jsonrpc":"2.0","id":81,"method":"tools/list"}`)
	if listHasTool(t, resp2, "work_item_search") {
		t.Fatalf("work_item_search must be absent when no searcher is wired")
	}
}

func listHasTool(t *testing.T, resp jsonrpcResponse, name string) bool {
	t.Helper()
	b, _ := json.Marshal(resp.Result)
	var r struct {
		Tools []mcpTool `json:"tools"`
	}
	_ = json.Unmarshal(b, &r)
	for _, tl := range r.Tools {
		if tl.Name == name {
			if len(tl.InputSchema) == 0 {
				t.Fatalf("tool %q has empty inputSchema", name)
			}
			return true
		}
	}
	return false
}

// TestMCP_ToolsCall_WorkItemSearchScopesTeamFromHeader asserts the tool binds the RBAC scope from the
// server-authenticated session (sess.team) — never from arguments — and never widens to AllTeams.
func TestMCP_ToolsCall_WorkItemSearchScopesTeamFromHeader(t *testing.T) {
	finder := &fakeWorkItemSearcher{results: []search.Result{{
		Type: "work_item", ID: "id-1", ProjectID: testProjectID, Title: "Fix the thing",
		Snippet: "fix the <mark>thing</mark>", State: "in_progress", Rank: 0.5, UpdatedAt: time.Unix(0, 0).UTC(),
	}}}
	mux, _ := mountMCPWithSearcher(finder)
	body := `{"jsonrpc":"2.0","id":82,"method":"tools/call","params":{"name":"work_item_search","arguments":{"query":"thing","limit":5}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": testTeamID}, body)
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}
	text, isErr := resultContentText(t, resp.Result)
	if isErr {
		t.Fatalf("unexpected tool error: %s", text)
	}
	// RBAC scope bound from the header, fenced to the Team, never fleet-wide.
	if finder.got.TeamID != testTeamID {
		t.Fatalf("TeamID = %q, want header-bound %q", finder.got.TeamID, testTeamID)
	}
	if finder.got.AllTeams {
		t.Fatalf("AllTeams must be false for an MCP (team-scoped agent) caller")
	}
	if finder.got.Text != "thing" || finder.got.Limit != 5 {
		t.Fatalf("query/limit not threaded: %+v", finder.got)
	}
	var out workItemSearchResponse
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode result: %v (text=%q)", err, text)
	}
	if len(out.Results) != 1 || out.Results[0].ID != "id-1" {
		t.Fatalf("results not surfaced: %+v", out.Results)
	}
}

// TestMCP_ToolsCall_WorkItemSearchForgedTeamIgnored asserts a team passed in the arguments cannot widen
// the scope — only the server-authenticated header is honoured (INV3 / ADR-039).
func TestMCP_ToolsCall_WorkItemSearchForgedTeamIgnored(t *testing.T) {
	finder := &fakeWorkItemSearcher{}
	mux, _ := mountMCPWithSearcher(finder)
	// team_id / all_teams in the arguments must be ignored (they are not even schema fields).
	body := `{"jsonrpc":"2.0","id":83,"method":"tools/call","params":{"name":"work_item_search","arguments":{"query":"x","team_id":"cccccccc-cccc-cccc-cccc-cccccccccccc","all_teams":true}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": testTeamID}, body)
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}
	if finder.got.TeamID != testTeamID || finder.got.AllTeams {
		t.Fatalf("argument-supplied scope leaked: TeamID=%q AllTeams=%v", finder.got.TeamID, finder.got.AllTeams)
	}
}

// TestMCP_ToolsCall_WorkItemSearchMissingTeamIsToolError asserts the tool refuses a call with no
// server-authenticated Team, exactly like the other tools (the MCP edge of INV3).
func TestMCP_ToolsCall_WorkItemSearchMissingTeamIsToolError(t *testing.T) {
	mux, _ := mountMCPWithSearcher(&fakeWorkItemSearcher{})
	body := `{"jsonrpc":"2.0","id":84,"method":"tools/call","params":{"name":"work_item_search","arguments":{"query":"x"}}}`
	resp := rpcCall(t, mux, nil, body)
	text, isErr := resultContentText(t, resp.Result)
	if !isErr || !strings.Contains(text, "X-Team-Id") {
		t.Fatalf("want missing-team tool error, got isErr=%v text=%q", isErr, text)
	}
}

// TestMCP_ToolsCall_WorkItemSearchEmptyQueryIsToolError asserts a blank query is surfaced as a tool error
// (mirroring the HTTP handler's 400) rather than a protocol error.
func TestMCP_ToolsCall_WorkItemSearchEmptyQueryIsToolError(t *testing.T) {
	mux, _ := mountMCPWithSearcher(&fakeWorkItemSearcher{err: search.ErrEmptyQuery})
	body := `{"jsonrpc":"2.0","id":85,"method":"tools/call","params":{"name":"work_item_search","arguments":{"query":"   "}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": testTeamID}, body)
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}
	_, isErr := resultContentText(t, resp.Result)
	if !isErr {
		t.Fatalf("want isError=true for an empty query")
	}
}

// TestMCP_ToolsCall_WorkItemSearchUnmountedWhenNil asserts a DB-less deployment reports work_item_search
// as an unknown tool — it never silently accepts a call it cannot serve.
func TestMCP_ToolsCall_WorkItemSearchUnmountedWhenNil(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	body := `{"jsonrpc":"2.0","id":86,"method":"tools/call","params":{"name":"work_item_search","arguments":{"query":"x"}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": testTeamID}, body)
	text, isErr := resultContentText(t, resp.Result)
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("want unknown-tool isError, got isErr=%v text=%q", isErr, text)
	}
}
