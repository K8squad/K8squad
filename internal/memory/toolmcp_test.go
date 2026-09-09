package memory

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rpcCall posts one JSON-RPC request to the MCP endpoint with the given headers and returns the decoded
// response envelope. A helper so each test reads as one protocol exchange.
func rpcCall(t *testing.T, mux *http.ServeMux, headers map[string]string, body string) jsonrpcResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, MCPEndpoint, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	mux.ServeHTTP(rec, req)
	var resp jsonrpcResponse
	if rec.Body.Len() > 0 {
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode rpc response: %v (body=%q)", err, rec.Body.String())
		}
	}
	return resp
}

// resultContentText pulls the single text content block out of a tools/call result. Fails the test if
// the result is not a well-formed MCP tool result.
func resultContentText(t *testing.T, result any) (string, bool) {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var parsed struct {
		Content []mcpContent `json:"content"`
		IsError bool         `json:"isError"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(parsed.Content) == 0 {
		t.Fatalf("result has no content: %s", string(b))
	}
	return parsed.Content[0].Text, parsed.IsError
}

func mountMCP(read *ReadService, write *WriteService) *http.ServeMux {
	mux := http.NewServeMux()
	NewToolMCP(read, write).Mount(mux)
	return mux
}

// TestMCP_Initialize asserts the handshake echoes the client protocol version and advertises the tools
// capability.
func TestMCP_Initialize(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	resp := rpcCall(t, mux, nil, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var r struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
	}
	_ = json.Unmarshal(b, &r)
	if r.ProtocolVersion != "2025-06-18" {
		t.Fatalf("protocolVersion = %q, want echoed 2025-06-18", r.ProtocolVersion)
	}
	if r.Capabilities.Tools == nil {
		t.Fatalf("initialize did not advertise tools capability: %s", string(b))
	}
}

// TestMCP_ToolsList_ReadOnlyOmitsWrite asserts a read-only deployment (nil write service) advertises
// exactly memory_search + discussion_search and NOT memory_write — the catalog matches what tools/call
// will actually serve.
func TestMCP_ToolsList_ReadOnlyOmitsWrite(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
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
		if len(tl.InputSchema) == 0 {
			t.Fatalf("tool %q has empty inputSchema", tl.Name)
		}
	}
	if !names["memory_search"] || !names["discussion_search"] {
		t.Fatalf("read tools missing from catalog: %v", names)
	}
	if names["memory_write"] {
		t.Fatalf("memory_write must be absent when no write service is wired")
	}
}

// TestMCP_ToolsList_WithWriteAdvertisesFullSurface asserts a full deployment advertises the whole MVP
// tool surface: the three read/write tools plus the diary ergonomics (diary_read + diary_append).
func TestMCP_ToolsList_WithWriteAdvertisesFullSurface(t *testing.T) {
	mux := mountMCP(
		NewReadService(&fakeSearcher{}, NewHashingEmbedder()),
		NewWriteService(&fakeWriter{}, NewHashingEmbedder()),
	)
	resp := rpcCall(t, mux, nil, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	b, _ := json.Marshal(resp.Result)
	var r struct {
		Tools []mcpTool `json:"tools"`
	}
	_ = json.Unmarshal(b, &r)
	names := map[string]bool{}
	for _, tl := range r.Tools {
		names[tl.Name] = true
		if len(tl.InputSchema) == 0 {
			t.Fatalf("tool %q has empty inputSchema", tl.Name)
		}
	}
	for _, want := range []string{"memory_search", "discussion_search", "diary_read", "memory_write", "diary_append"} {
		if !names[want] {
			t.Fatalf("tool %q missing from full catalog: %v", want, names)
		}
	}
	if len(r.Tools) != 5 {
		t.Fatalf("want 5 tools, got %d (%v)", len(r.Tools), names)
	}
}

// TestMCP_ToolsList_ReadOnlyAdvertisesDiaryRead asserts a read-only deployment (nil write service) still
// advertises diary_read (a read) but NOT diary_append (a write) — the catalog matches what it will serve.
func TestMCP_ToolsList_ReadOnlyAdvertisesDiaryRead(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	resp := rpcCall(t, mux, nil, `{"jsonrpc":"2.0","id":31,"method":"tools/list"}`)
	b, _ := json.Marshal(resp.Result)
	var r struct {
		Tools []mcpTool `json:"tools"`
	}
	_ = json.Unmarshal(b, &r)
	names := map[string]bool{}
	for _, tl := range r.Tools {
		names[tl.Name] = true
	}
	if !names["diary_read"] {
		t.Fatalf("diary_read (a read tool) must be advertised read-only: %v", names)
	}
	if names["diary_append"] {
		t.Fatalf("diary_append (a write tool) must be absent when no write service is wired: %v", names)
	}
}

// TestMCP_ToolsCall_TeamFromHeaderNotArgs is the MCP edge of INV3: discussion_search scopes to the
// X-Team-Id header, never a team smuggled in the arguments object. The recorded SearchQuery must carry
// the HEADER team.
func TestMCP_ToolsCall_TeamFromHeaderNotArgs(t *testing.T) {
	fake := &fakeSearcher{}
	mux := mountMCP(NewReadService(fake, NewHashingEmbedder()), nil)
	// The arguments try to smuggle a different team; the transport must ignore it and use the header.
	body := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"discussion_search","arguments":{"project_id":"proj-A","query":"deploy","top_k":5,"team_id":"attacker-team"}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1"}, body)
	if resp.Error != nil {
		t.Fatalf("tools/call error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("tools/call reported tool error unexpectedly")
	}
	if fake.got.SquadID != "team-1" {
		t.Fatalf("SquadID = %q, want team-1 (from header, never args)", fake.got.SquadID)
	}
	if fake.got.ProjectID == nil || *fake.got.ProjectID != "proj-A" {
		t.Fatalf("ProjectID = %v, want proj-A", fake.got.ProjectID)
	}
}

// TestMCP_ToolsCall_MissingTeamHeaderIsToolError asserts a call with no X-Team-Id returns a tool-level
// error (isError=true), not a silent success — mirrors toolhttp's 401.
func TestMCP_ToolsCall_MissingTeamHeaderIsToolError(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	body := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"q"}}}`
	resp := rpcCall(t, mux, nil, body)
	if resp.Error != nil {
		t.Fatalf("expected a well-formed result with isError, got protocol error: %+v", resp.Error)
	}
	text, isErr := resultContentText(t, resp.Result)
	if !isErr {
		t.Fatalf("want isError=true for missing team header, got success: %q", text)
	}
}

// TestMCP_ToolsCall_WriteStampsAuthorFromHeaders is the write-path edge of WINV1/WINV2: memory_write
// stamps tenancy and authorship from the session headers, never the arguments. A body smuggling
// team_id/principal is ignored; the recorded WriteRequest carries the HEADER identity.
func TestMCP_ToolsCall_WriteStampsAuthorFromHeaders(t *testing.T) {
	fw := &fakeWriter{}
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), NewWriteService(fw, NewHashingEmbedder()))
	args := `{"kind":"diary","content":"rendered metallb pool, opened PR","project_id":"proj-A","team_id":"attacker","principal":"attacker"}`
	body := `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"memory_write","arguments":` + args + `}}`
	headers := map[string]string{
		"X-Team-Id":      "team-1",
		"X-Principal-Id": "agent:coder",
		"X-Agent-Id":     "agent-uuid",
		"X-Run-Id":       "run-uuid",
	}
	resp := rpcCall(t, mux, headers, body)
	if resp.Error != nil {
		t.Fatalf("tools/call error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("write reported tool error unexpectedly")
	}
	if fw.got.SquadID != "team-1" {
		t.Fatalf("SquadID = %q, want team-1 (from header)", fw.got.SquadID)
	}
	if fw.got.PrincipalID != "agent:coder" {
		t.Fatalf("PrincipalID = %q, want agent:coder (from header)", fw.got.PrincipalID)
	}
	if fw.got.AgentID == nil || *fw.got.AgentID != "agent-uuid" {
		t.Fatalf("AgentID = %v, want agent-uuid (from header)", fw.got.AgentID)
	}
	if fw.got.RunID == nil || *fw.got.RunID != "run-uuid" {
		t.Fatalf("RunID = %v, want run-uuid (from header)", fw.got.RunID)
	}
	if fw.got.Kind != KindDiary {
		t.Fatalf("Kind = %q, want diary", fw.got.Kind)
	}
	if fw.got.ProjectID == nil || *fw.got.ProjectID != "proj-A" {
		t.Fatalf("ProjectID = %v, want proj-A", fw.got.ProjectID)
	}
}

// TestMCP_ToolsCall_WriteRequiresPrincipal asserts a memory_write missing X-Principal-Id is a tool error
// even when the team header is present (an unauthenticated author).
func TestMCP_ToolsCall_WriteRequiresPrincipal(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), NewWriteService(&fakeWriter{}, NewHashingEmbedder()))
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"memory_write","arguments":{"content":"x"}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1"}, body)
	if resp.Error != nil {
		t.Fatalf("expected tool error result, got protocol error: %+v", resp.Error)
	}
	_, isErr := resultContentText(t, resp.Result)
	if !isErr {
		t.Fatalf("want isError=true without X-Principal-Id")
	}
}

// TestMCP_ToolsCall_WriteUnmountedWhenNil asserts a read-only deployment reports memory_write as an
// unknown tool (never silently accepts a write it cannot serve).
func TestMCP_ToolsCall_WriteUnmountedWhenNil(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	body := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"memory_write","arguments":{"content":"x"}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1", "X-Principal-Id": "p"}, body)
	if resp.Error != nil {
		t.Fatalf("expected tool error result, got protocol error: %+v", resp.Error)
	}
	text, isErr := resultContentText(t, resp.Result)
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("want unknown-tool isError, got isErr=%v text=%q", isErr, text)
	}
}

// TestMCP_ToolsCall_DiaryReadScopesTeamAgentKind is the diary_read read-plan assertion: the caller team
// comes from X-Team-Id (never args, INV3), the requested agent + fixed kind=diary narrow the
// chronological read, and last_n threads through as the limit.
func TestMCP_ToolsCall_DiaryReadScopesTeamAgentKind(t *testing.T) {
	fake := &fakeSearcher{}
	mux := mountMCP(NewReadService(fake, NewHashingEmbedder()), nil)
	// A body that tries to smuggle a different team must be ignored; the header team wins.
	body := `{"jsonrpc":"2.0","id":40,"method":"tools/call","params":{"name":"diary_read","arguments":{"agent":"agent-42","last_n":3,"team_id":"attacker"}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1"}, body)
	if resp.Error != nil {
		t.Fatalf("tools/call error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("diary_read reported tool error unexpectedly")
	}
	if fake.chronoSquad != "team-1" {
		t.Fatalf("chrono squad = %q, want team-1 (from header, never args)", fake.chronoSquad)
	}
	if fake.chronoAgent != "agent-42" {
		t.Fatalf("chrono agent = %q, want agent-42", fake.chronoAgent)
	}
	if fake.chronoKind != KindDiary {
		t.Fatalf("chrono kind = %q, want diary", fake.chronoKind)
	}
	if fake.chronoLimit != 3 {
		t.Fatalf("chrono limit = %d, want 3 (from last_n)", fake.chronoLimit)
	}
}

// TestMCP_ToolsCall_DiaryReadMissingTeamIsToolError asserts diary_read without X-Team-Id is a tool error.
func TestMCP_ToolsCall_DiaryReadMissingTeamIsToolError(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	body := `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"diary_read","arguments":{"agent":"a"}}}`
	resp := rpcCall(t, mux, nil, body)
	if resp.Error != nil {
		t.Fatalf("expected tool error result, got protocol error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); !isErr {
		t.Fatalf("want isError=true for missing team header")
	}
}

// TestMCP_ToolsCall_DiaryReadRequiresAgent asserts diary_read without an `agent` arg is a tool error
// (the diary owner is required — there is no "read everyone's diary" widening).
func TestMCP_ToolsCall_DiaryReadRequiresAgent(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	body := `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"diary_read","arguments":{"last_n":5}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1"}, body)
	if resp.Error != nil {
		t.Fatalf("expected tool error result, got protocol error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); !isErr {
		t.Fatalf("want isError=true when agent is missing")
	}
}

// TestMCP_ToolsCall_DiaryAppendStampsAuthorFixedKind is the diary_append edge: it stamps tenancy +
// authorship from the session headers (never args) and FIXES kind=diary. A body smuggling kind/project_id
// is inert — diary_append takes only `entry`.
func TestMCP_ToolsCall_DiaryAppendStampsAuthorFixedKind(t *testing.T) {
	fw := &fakeWriter{}
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), NewWriteService(fw, NewHashingEmbedder()))
	// entry is the only honored arg; kind/project_id here must NOT reach the write path.
	args := `{"entry":"rendered metallb pool, opened PR","kind":"fact","project_id":"proj-A"}`
	body := `{"jsonrpc":"2.0","id":43,"method":"tools/call","params":{"name":"diary_append","arguments":` + args + `}}`
	headers := map[string]string{
		"X-Team-Id":      "team-1",
		"X-Principal-Id": "agent:coder",
		"X-Agent-Id":     "agent-uuid",
		"X-Run-Id":       "run-uuid",
	}
	resp := rpcCall(t, mux, headers, body)
	if resp.Error != nil {
		t.Fatalf("tools/call error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("diary_append reported tool error unexpectedly")
	}
	if fw.got.SquadID != "team-1" {
		t.Fatalf("SquadID = %q, want team-1 (from header)", fw.got.SquadID)
	}
	if fw.got.PrincipalID != "agent:coder" {
		t.Fatalf("PrincipalID = %q, want agent:coder (from header)", fw.got.PrincipalID)
	}
	if fw.got.Kind != KindDiary {
		t.Fatalf("Kind = %q, want diary (fixed, ignoring the smuggled kind=fact)", fw.got.Kind)
	}
	if fw.got.ProjectID != nil {
		t.Fatalf("ProjectID = %v, want nil (diary_append takes no project_id)", fw.got.ProjectID)
	}
}

// TestMCP_ToolsCall_DiaryAppendRequiresPrincipal asserts diary_append missing X-Principal-Id is a tool
// error even with a team header (an unauthenticated author cannot append).
func TestMCP_ToolsCall_DiaryAppendRequiresPrincipal(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), NewWriteService(&fakeWriter{}, NewHashingEmbedder()))
	body := `{"jsonrpc":"2.0","id":44,"method":"tools/call","params":{"name":"diary_append","arguments":{"entry":"x"}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1"}, body)
	if resp.Error != nil {
		t.Fatalf("expected tool error result, got protocol error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); !isErr {
		t.Fatalf("want isError=true without X-Principal-Id")
	}
}

// TestMCP_ToolsCall_DiaryAppendUnmountedWhenNil asserts a read-only deployment reports diary_append as an
// unknown tool (never silently accepts a write it cannot serve).
func TestMCP_ToolsCall_DiaryAppendUnmountedWhenNil(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	body := `{"jsonrpc":"2.0","id":45,"method":"tools/call","params":{"name":"diary_append","arguments":{"entry":"x"}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1", "X-Principal-Id": "p"}, body)
	if resp.Error != nil {
		t.Fatalf("expected tool error result, got protocol error: %+v", resp.Error)
	}
	text, isErr := resultContentText(t, resp.Result)
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("want unknown-tool isError, got isErr=%v text=%q", isErr, text)
	}
}

// TestMCP_UnknownMethodIsMethodNotFound asserts an unrecognized JSON-RPC method returns the reserved
// -32601 protocol error.
func TestMCP_UnknownMethodIsMethodNotFound(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	resp := rpcCall(t, mux, nil, `{"jsonrpc":"2.0","id":9,"method":"does/not/exist"}`)
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Fatalf("want method-not-found (-32601), got %+v", resp.Error)
	}
}

// TestMCP_NotificationGetsNoBody asserts a notification (no id) produces no JSON-RPC reply body — the
// spec forbids replying to a notification.
func TestMCP_NotificationGetsNoBody(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, MCPEndpoint, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 Accepted for a notification", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("notification produced a body: %q", rec.Body.String())
	}
}

// TestMCP_ParseErrorOnBadJSON asserts a malformed JSON body yields the reserved -32700 parse error.
func TestMCP_ParseErrorOnBadJSON(t *testing.T) {
	mux := mountMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, MCPEndpoint, bytes.NewReader([]byte(`{not json`)))
	mux.ServeHTTP(rec, req)
	var resp jsonrpcResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != codeParseError {
		t.Fatalf("want parse error (-32700), got %+v", resp.Error)
	}
}
