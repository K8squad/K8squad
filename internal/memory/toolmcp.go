package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ToolMCP is the MCP JSON-RPC 2.0 transport for the memory tool surface (Story 6.2 / ISI-3179). It is
// the wire protocol the operator's MCPServer probe (pkg/controller/mcpserver) and any MCP-speaking agent
// actually talk: a single streamable-HTTP endpoint that serves `initialize`, `tools/list`, and
// `tools/call` over JSON-RPC 2.0, registering the SAME transport-agnostic ReadService + WriteService the
// thin JSON/HTTP shim (toolhttp.go) exposes. The tool logic is the ReadService/WriteService, not this
// transport — 6.2 is only the wire protocol + tool schema/registry, plus the server-auth scoping.
//
// SERVER-AUTH SCOPING (INV3 / WINV1/WINV2). The caller's tenancy and authorship are taken from the
// server-authenticated headers stamped by the §13 BFF — X-Team-Id (tenant), X-Principal-Id (author),
// and the optional X-Agent-Id / X-Run-Id — exactly as toolhttp.go and the discussion apiserver's
// headerAuth do. They are threaded into the per-request MCP session context (mcpSession) and are NEVER
// read from a tool's JSON-RPC params: a caller cannot widen past its tenant or forge authorship through
// the arguments object. project_id / query / top_k / kind / content / provenance are the only tool args.
type ToolMCP struct {
	read    *ReadService
	write   *WriteService
	discuss DiscussionWriter
}

// NewToolMCP wires the MCP transport to a ReadService and (optionally) a WriteService plus a
// DiscussionWriter. A nil write service leaves memory_write / diary_append out of the registry; a nil
// discuss writer leaves discussion_post out — a read-only deployment advertises and serves only the read
// tools, exactly as NewToolHTTP leaves the write tools unmounted. The discuss seam is the SAME
// *discussion.Store the thin HTTP shim (toolhttp.go) and the REST handler use, so provenance-stamping
// and Team-scope enforcement (the fence) live in the store, not this transport (ISI-4085, ISI-4013 AC3).
func NewToolMCP(read *ReadService, write *WriteService, discuss DiscussionWriter) *ToolMCP {
	return &ToolMCP{read: read, write: write, discuss: discuss}
}

// MCPEndpoint is the single streamable-HTTP path the JSON-RPC transport is served on. It sits alongside
// the thin per-tool JSON/HTTP routes (/mcp/tools/*) rather than replacing them, so a compatibility
// window has both surfaces live off the one ReadService/WriteService.
const MCPEndpoint = "/mcp"

// mcpProtocolVersion is the MCP protocol revision this server implements. The initialize handshake
// echoes the client's requested version when we support it, else falls back to this.
const mcpProtocolVersion = "2025-06-18"

// Mount registers the JSON-RPC endpoint on the given mux.
func (m *ToolMCP) Mount(mux *http.ServeMux) {
	mux.HandleFunc(MCPEndpoint, m.handle)
}

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 envelopes (mirrors the shapes the operator's MCP probe speaks in
// pkg/controller/mcpserver/probe.go — the client and server halves of one protocol).
// ---------------------------------------------------------------------------

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent ⇒ a notification (no reply)
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC 2.0 reserved error codes used by this transport.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// mcpSession is the per-request server-authenticated scope threaded from the transport headers into the
// tool handlers. It is the MCP analogue of toolhttp's per-request header reads: un-spoofable knowledge
// carried from auth to the ReadService/WriteService, never from a tool's arguments.
type mcpSession struct {
	team      string  // X-Team-Id — required for every tool call (tenant root)
	principal string  // X-Principal-Id — required only for memory_write (author)
	agentID   *string // X-Agent-Id — optional authorship linkage
	runID     *string // X-Run-Id — optional Run linkage
}

// handle is the single JSON-RPC entrypoint. It decodes the envelope, dispatches by method, and writes a
// JSON-RPC reply (or nothing for a notification). Tenancy/authorship come from the request headers into
// mcpSession — the JSON-RPC params never carry them.
func (m *ToolMCP) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := mcpSession{
		team:      r.Header.Get("X-Team-Id"),
		principal: r.Header.Get("X-Principal-Id"),
		agentID:   optional(r.Header.Get("X-Agent-Id")),
		runID:     optional(r.Header.Get("X-Run-Id")),
	}

	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPC(w, jsonrpcResponse{JSONRPC: "2.0", Error: &jsonrpcError{Code: codeParseError, Message: "parse error"}})
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPC(w, jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &jsonrpcError{Code: codeInvalidRequest, Message: "jsonrpc must be \"2.0\""}})
		return
	}

	// A notification (no id) gets no reply — MCP uses notifications/initialized as a fire-and-forget.
	isNotification := len(req.ID) == 0

	result, rpcErr := m.dispatch(r.Context(), sess, req.Method, req.Params)

	if isNotification {
		// Fire-and-forget: acknowledge at the HTTP layer, emit no JSON-RPC body (a reply to a
		// notification would violate the spec — there is no id to correlate it).
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := jsonrpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	writeRPC(w, resp)
}

// dispatch routes a JSON-RPC method to its handler. Protocol methods (initialize, notifications/*) are
// unscoped; tools/list is unscoped (a capability catalog, not a data read); tools/call is where the
// per-request tenancy/authorship scoping bites.
func (m *ToolMCP) dispatch(ctx context.Context, sess mcpSession, method string, params json.RawMessage) (any, *jsonrpcError) {
	switch method {
	case "initialize":
		return m.initialize(params), nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil // notification — no result
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return m.toolsList(), nil
	case "tools/call":
		return m.toolsCall(ctx, sess, params)
	default:
		return nil, &jsonrpcError{Code: codeMethodNotFound, Message: fmt.Sprintf("method not found: %s", method)}
	}
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// initialize is the MCP handshake. It advertises tools capability and echoes the client's requested
// protocol version when we implement it, else offers ours — the standard version-negotiation posture.
func (m *ToolMCP) initialize(params json.RawMessage) any {
	version := mcpProtocolVersion
	if len(params) > 0 {
		var p initializeParams
		if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    "ksquad-memory",
			"version": "0.6.2",
		},
	}
}

// ---------------------------------------------------------------------------
// tools/list — the tool schema/registry
// ---------------------------------------------------------------------------

// mcpTool is one entry in the tools/list catalog (name + human description + JSON Schema for arguments).
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// toolsList advertises the memory tools. The read tools (memory_search, discussion_search, diary_read)
// are always present; the memory write tools (memory_write, diary_append) are omitted when no
// WriteService is wired, and discussion_post is omitted when no DiscussionWriter is wired (a read-only
// deployment), so a client's catalog matches exactly what tools/call will serve.
func (m *ToolMCP) toolsList() any {
	tools := []mcpTool{memorySearchTool, discussionSearchTool, diaryReadTool}
	if m.write != nil {
		tools = append(tools, memoryWriteTool, diaryAppendTool)
	}
	if m.discuss != nil {
		tools = append(tools, discussionPostTool)
	}
	return map[string]any{"tools": tools}
}

// The tool schemas are the wire contract. Tenancy (team) and authorship (principal/agent/run) are
// DELIBERATELY absent from every inputSchema: they are server-authenticated transport headers, not tool
// arguments, so the schema cannot even express a request to widen tenancy or forge authorship (INV3).
var (
	memorySearchTool = mcpTool{
		Name:        "memory_search",
		Description: "Semantic search over the caller team's untrusted knowledge memory. Returns untrusted-provenance envelopes {content, author, written_at, scope, trust}. The caller's team is server-authenticated, never an argument.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"natural-language search text"},"top_k":{"type":"integer","description":"max results (default server-chosen)","minimum":1}},"required":["query"]}`),
	}
	discussionSearchTool = mcpTool{
		Name:        "discussion_search",
		Description: "Semantic search over a project's indexed discussion messages, scoped to the caller team. Returns untrusted-provenance envelopes. team is server-authenticated; project_id narrows within the team.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"string","description":"project to scope the search to (narrows within the caller team)"},"query":{"type":"string","description":"natural-language search text"},"top_k":{"type":"integer","description":"max results (default server-chosen)","minimum":1}},"required":["project_id","query"]}`),
	}
	memoryWriteTool = mcpTool{
		Name:        "memory_write",
		Description: "Commit one knowledge record into the caller team's memory. Author and tenancy are server-authenticated (never arguments). kind is one of note|fact|diary (default note); a diary entry is a chronological first-person work-log record recalled like any other untrusted knowledge.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","enum":["note","fact","diary"],"description":"record kind (default note); diary is an agent work-log entry"},"content":{"type":"string","description":"the record body; embedded and stored"},"project_id":{"type":"string","description":"optional narrower scope within the caller team"},"provenance":{"type":"object","description":"opaque caller metadata (never consulted for trust or authorship)"}},"required":["content"]}`),
	}
	// diaryReadTool is the chronological (NOT semantic) read: an agent's last N diary entries in time
	// order. It is always advertised (a read tool); `agent` is the diary owner, team is server-authed.
	diaryReadTool = mcpTool{
		Name:        "diary_read",
		Description: "Read an agent's most recent diary entries, chronological (newest first), scoped to the caller team. Returns untrusted-provenance envelopes {content, author, written_at, scope, trust}. team is server-authenticated; agent is the diary owner's id; last_n bounds the count (default server-chosen).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string","description":"the diary owner's agent id (author_agent_id)"},"last_n":{"type":"integer","description":"max entries, newest first (default server-chosen)","minimum":1}},"required":["agent"]}`),
	}
	// diaryAppendTool is dedicated sugar over memory_write with a FIXED kind=diary and no kind/project_id
	// args. Advertised only when a WriteService is wired, exactly like memory_write.
	diaryAppendTool = mcpTool{
		Name:        "diary_append",
		Description: "Append one entry to the calling agent's own diary — a chronological first-person work-log record in the caller team. Author and tenancy are server-authenticated (never arguments); kind is fixed to diary. Returns the server-assigned record id.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"entry":{"type":"string","description":"the diary entry body; embedded and stored"}},"required":["entry"]}`),
	}
	// discussionPostTool is the authored write peer of discussion_search: it opens a thread or replies
	// into one in a project's discussion room. Authorship + tenancy are server-authenticated (team from
	// X-Team-Id, principal/agent/run from their headers) and NEVER arguments (WINV1/WINV2); thread_id
	// presence switches open-vs-reply. Advertised only when a DiscussionWriter is wired.
	discussionPostTool = mcpTool{
		Name:        "discussion_post",
		Description: "Post to a project's discussion room, scoped to the caller team. Omit thread_id to open a new thread (title required); pass thread_id to reply into it (optionally parent_message_id to nest). Author and tenancy are server-authenticated (never arguments). Returns the server-stamped thread (on open) or message (on reply) so you can cite what you wrote.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"string","description":"the discussion room's project id (uuid); narrows within the caller team"},"thread_id":{"type":"string","description":"omit to open a new thread; present (uuid) to reply into that thread"},"title":{"type":"string","description":"thread title, required when opening a new thread (thread_id omitted)"},"body":{"type":"string","description":"the thread's first message (on open) or the reply body"},"parent_message_id":{"type":"string","description":"optional reply target (uuid) within the thread; ignored when opening"}},"required":["project_id","body"]}`),
	}
)

// ---------------------------------------------------------------------------
// tools/call — dispatch into the ReadService / WriteService with header-derived scope
// ---------------------------------------------------------------------------

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// mcpContent is one item of a tools/call result's content array (§ MCP tool result). We return the tool
// output as a single JSON text block — the structured envelope/record marshaled into text — which every
// MCP client can render, plus isError for a tool-level failure (distinct from a protocol JSON-RPC error).
type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func toolResult(payload any) (any, *jsonrpcError) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, &jsonrpcError{Code: codeInternalError, Message: "marshal tool result"}
	}
	return map[string]any{"content": []mcpContent{{Type: "text", Text: string(b)}}}, nil
}

// toolError returns an MCP tool-execution error: a well-formed result with isError=true, NOT a JSON-RPC
// protocol error. This is the MCP convention — the call itself succeeded at the protocol level; the tool
// reported a domain failure (bad args, tenancy missing) the model is meant to see and react to.
func toolError(msg string) (any, *jsonrpcError) {
	return map[string]any{
		"isError": true,
		"content": []mcpContent{{Type: "text", Text: msg}},
	}, nil
}

func (m *ToolMCP) toolsCall(ctx context.Context, sess mcpSession, params json.RawMessage) (any, *jsonrpcError) {
	var p toolsCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &jsonrpcError{Code: codeInvalidParams, Message: "invalid tools/call params"}
	}
	// Every tool is tenant-scoped: without a server-authenticated team there is no caller identity to
	// scope the read/write to. This is the MCP edge of INV3 — mirrors toolhttp's 401 on a missing header.
	if sess.team == "" {
		return toolError("X-Team-Id required (server-authenticated caller tenant)")
	}

	switch p.Name {
	case memorySearchTool.Name:
		return m.callMemorySearch(ctx, sess, p.Arguments)
	case discussionSearchTool.Name:
		return m.callDiscussionSearch(ctx, sess, p.Arguments)
	case diaryReadTool.Name:
		return m.callDiaryRead(ctx, sess, p.Arguments)
	case memoryWriteTool.Name:
		if m.write == nil {
			return toolError(fmt.Sprintf("unknown tool: %s", p.Name)) // unmounted in a read-only deployment
		}
		return m.callMemoryWrite(ctx, sess, p.Arguments)
	case diaryAppendTool.Name:
		if m.write == nil {
			return toolError(fmt.Sprintf("unknown tool: %s", p.Name)) // a write — unmounted read-only
		}
		return m.callDiaryAppend(ctx, sess, p.Arguments)
	case discussionPostTool.Name:
		if m.discuss == nil {
			return toolError(fmt.Sprintf("unknown tool: %s", p.Name)) // a write — unmounted read-only
		}
		return m.callDiscussionPost(ctx, sess, p.Arguments)
	default:
		return toolError(fmt.Sprintf("unknown tool: %s", p.Name))
	}
}

// searchArgs is the shared read-tool argument shape. team is DELIBERATELY absent — it comes from the
// session (header), never here.
type searchArgs struct {
	ProjectID string `json:"project_id,omitempty"`
	Query     string `json:"query"`
	TopK      int    `json:"top_k,omitempty"`
}

func (m *ToolMCP) callMemorySearch(ctx context.Context, sess mcpSession, raw json.RawMessage) (any, *jsonrpcError) {
	var a searchArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return toolError("invalid arguments")
		}
	}
	out, err := m.read.MemorySearch(ctx, sess.team, a.Query, a.TopK)
	if err != nil {
		return toolError(err.Error())
	}
	return toolResult(searchResponse{Results: out})
}

func (m *ToolMCP) callDiscussionSearch(ctx context.Context, sess mcpSession, raw json.RawMessage) (any, *jsonrpcError) {
	var a searchArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return toolError("invalid arguments")
		}
	}
	out, err := m.read.DiscussionSearch(ctx, sess.team, a.ProjectID, a.Query, a.TopK)
	if err != nil {
		return toolError(err.Error())
	}
	return toolResult(searchResponse{Results: out})
}

// diaryReadArgs is the diary_read argument shape. team is absent — it comes from the session (header),
// never here (INV3). `agent` is the diary owner; `last_n` bounds the chronological read.
type diaryReadArgs struct {
	Agent string `json:"agent"`
	LastN int    `json:"last_n,omitempty"`
}

func (m *ToolMCP) callDiaryRead(ctx context.Context, sess mcpSession, raw json.RawMessage) (any, *jsonrpcError) {
	var a diaryReadArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return toolError("invalid arguments")
		}
	}
	out, err := m.read.DiaryRead(ctx, sess.team, a.Agent, a.LastN)
	if err != nil {
		return toolError(err.Error())
	}
	return toolResult(searchResponse{Results: out})
}

// diaryAppendArgs is the diary_append argument shape. Scope/authorship/kind are absent by construction:
// team + principal + agent + run ride the session headers (WINV1/WINV2) and kind is fixed to diary.
type diaryAppendArgs struct {
	Entry string `json:"entry"`
}

func (m *ToolMCP) callDiaryAppend(ctx context.Context, sess mcpSession, raw json.RawMessage) (any, *jsonrpcError) {
	if sess.principal == "" {
		return toolError("X-Principal-Id required (server-authenticated author)")
	}
	var a diaryAppendArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return toolError("invalid arguments")
		}
	}
	author := AuthorScope{
		TeamID:    sess.team,
		Principal: sess.principal,
		AgentID:   sess.agentID,
		RunID:     sess.runID,
	}
	rec, err := m.write.DiaryAppend(ctx, author, a.Entry)
	if err != nil {
		return toolError(err.Error())
	}
	resp := writeToolResponse{ID: rec.ID, Kind: rec.Kind, TeamID: rec.SquadID}
	if rec.ProjectID != nil {
		resp.ProjectID = *rec.ProjectID
	}
	return toolResult(resp)
}

// writeArgs is the memory_write argument shape. Scope/authorship are absent by construction (WINV1/
// WINV2): team, principal, agent, and run ride the session headers. project_id (a narrowing scope),
// kind, content, and opaque provenance are the only body args — identical to toolhttp's writeToolRequest.
type writeArgs struct {
	ProjectID  string          `json:"project_id,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Content    string          `json:"content"`
	Provenance json.RawMessage `json:"provenance,omitempty"`
}

func (m *ToolMCP) callMemoryWrite(ctx context.Context, sess mcpSession, raw json.RawMessage) (any, *jsonrpcError) {
	if sess.principal == "" {
		return toolError("X-Principal-Id required (server-authenticated author)")
	}
	var a writeArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return toolError("invalid arguments")
		}
	}
	author := AuthorScope{
		TeamID:    sess.team,
		Principal: sess.principal,
		AgentID:   sess.agentID,
		RunID:     sess.runID,
	}
	var projectID *string
	if a.ProjectID != "" {
		projectID = &a.ProjectID
	}
	rec, err := m.write.MemoryWrite(ctx, author, a.Kind, a.Content, projectID, a.Provenance)
	if err != nil {
		return toolError(err.Error())
	}
	resp := writeToolResponse{ID: rec.ID, Kind: rec.Kind, TeamID: rec.SquadID}
	if rec.ProjectID != nil {
		resp.ProjectID = *rec.ProjectID
	}
	return toolResult(resp)
}

// discussionPostArgs is the discussion_post argument shape. Like writeArgs, authorship/tenancy are
// DELIBERATELY absent — team (X-Team-Id), principal, agent, and run ride the session headers, never the
// arguments (WINV1/WINV2, ISI-4013 AC3). Only the scope (project_id/thread_id/parent_message_id) and the
// content (title/body) are args, so a forged author_*/created_by/team_id has no path to the stored row.
type discussionPostArgs struct {
	ProjectID       string `json:"project_id"`                  // required: the room to post into (uuid)
	ThreadID        string `json:"thread_id,omitempty"`         // omitted ⇒ open a new thread; present ⇒ reply
	Title           string `json:"title,omitempty"`             // required iff thread_id omitted
	Body            string `json:"body"`                        // thread first-message body, or the reply body
	ParentMessageID string `json:"parent_message_id,omitempty"` // reply target within the thread (ignored when opening)
}

// callDiscussionPost is the MCP edge of the discussion_post tool: the authored write peer of
// discussion_search. It builds a discussion.AuthorContext from the SAME server-stamped headers
// callMemoryWrite reads and calls the SAME fenced Store methods the REST handler and HTTP shim call
// (OpenThread / PostMessage) — the store, not this transport, is where provenance-stamping and
// Team-scope enforcement live, so the arguments can neither widen tenancy nor forge authorship (WINV1/
// WINV2, ISI-4013 AC3). thread_id-presence switches open-vs-reply. Returns the server-stamped
// Thread/Message so the agent can cite what it just wrote (AC6). Malformed uuids and store errors are
// MCP tool errors (isError=true), the MCP analogue of the HTTP shim's 400/404 mapping.
func (m *ToolMCP) callDiscussionPost(ctx context.Context, sess mcpSession, raw json.RawMessage) (any, *jsonrpcError) {
	if sess.principal == "" {
		return toolError("X-Principal-Id required (server-authenticated author)")
	}
	teamID, err := uuid.Parse(sess.team)
	if err != nil {
		return toolError("malformed X-Team-Id (want uuid)")
	}
	var a discussionPostArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return toolError("invalid arguments")
		}
	}
	projectID, err := uuid.Parse(a.ProjectID)
	if err != nil {
		return toolError("malformed project_id (want uuid)")
	}
	// Provenance is stamped from `auth` alone — the header identity — mirroring handler AC3. IsAdmin
	// stays false: posting needs no admin and retract is out of scope for the tool.
	auth := discussion.AuthorContext{
		Principal: sess.principal,
		TeamID:    teamID,
		AgentID:   sess.agentID,
		RunID:     sess.runID,
	}
	// thread_id omitted ⇒ open a new thread (title required); present ⇒ reply to it. Every store error
	// (tenancy/scope miss → ErrThreadNotFound, empty body/title) surfaces as an MCP tool error
	// (isError=true) — the MCP convention for a domain failure the model should see, the analogue of the
	// HTTP shim's writeDiscussionErr status mapping — never a JSON-RPC protocol error.
	if a.ThreadID == "" {
		thread, err := m.discuss.OpenThread(ctx, projectID, auth, a.Title, a.Body)
		if err != nil {
			return toolError(err.Error())
		}
		return toolResult(thread)
	}
	threadID, err := uuid.Parse(a.ThreadID)
	if err != nil {
		return toolError("malformed thread_id (want uuid)")
	}
	var parentID *uuid.UUID
	if a.ParentMessageID != "" {
		pid, err := uuid.Parse(a.ParentMessageID)
		if err != nil {
			return toolError("malformed parent_message_id (want uuid)")
		}
		parentID = &pid
	}
	msg, err := m.discuss.PostMessage(ctx, projectID, teamID, threadID, auth, a.Body, parentID)
	if err != nil {
		return toolError(err.Error())
	}
	return toolResult(msg)
}

// writeRPC serializes a JSON-RPC response envelope. A serialization failure degrades to a 500 — there is
// no partial-body path for a JSON-RPC reply.
func writeRPC(w http.ResponseWriter, resp jsonrpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}
