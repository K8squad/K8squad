/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package runtimes

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
)

// TestClaudeCard pins the claude-code adapter's declarative surface: the
// pinned CLI revision (Dockerfile.shim CLAUDE_VERSION lockstep, ADR-017), the
// Phase-1 API-key credential shape, the curated sonnet default, and the
// honest capability gaps (InteractivePrompt=false print mode,
// BYOModelEndpoint=false Anthropic-wire-only).
func TestClaudeCard(t *testing.T) {
	rt, err := Get(apiv1alpha1.RuntimeTypeClaudeCode)
	if err != nil {
		t.Fatalf("Get(claude-code) failed: %v", err)
	}
	if rt.CLIVersion() != "2.1.236" {
		t.Errorf("CLIVersion() = %q, want 2.1.236 (Dockerfile.shim CLAUDE_VERSION)", rt.CLIVersion())
	}
	if rt.CredentialShape() != ShapeAPIKey {
		t.Errorf("CredentialShape() = %v, want ShapeAPIKey (Phase 1, ISI-6697 plan §4)", rt.CredentialShape())
	}
	dm := rt.DefaultModel()
	if dm.ID != "claude-sonnet-5" {
		t.Errorf("DefaultModel().ID = %q, want curated claude-sonnet-5 (modelHints.ts)", dm.ID)
	}
	if dm.ContextWindow != 200000 {
		t.Errorf("DefaultModel().ContextWindow = %d, want 200000", dm.ContextWindow)
	}
	caps := rt.Capabilities()
	if !caps.Streaming || !caps.ToolCalls {
		t.Errorf("streaming/toolCalls must be true; caps=%+v", caps)
	}
	if caps.InteractivePrompt {
		t.Error("claude-code must advertise interactivePrompt=false (print mode is one-shot)")
	}
	if caps.BYOModelEndpoint {
		t.Error("claude-code must advertise byoModelEndpoint=false (Anthropic wire only, Phase 1)")
	}
	if !caps.RateLimitSignal {
		t.Error("claude-code must advertise rateLimitSignal=true (result parser emits EventRateLimited)")
	}
}

// TestClaudeCommand pins the headless invocation contract (the WS-1 risk
// area): print mode with the full stream-json event stream, the credential on
// ANTHROPIC_API_KEY (never argv), the whole prompt on stdin (never argv, no
// context env — ISI-5329), the model override, and the rendered .mcp.json
// workdir config with ${ENV} credential references.
func TestClaudeCommand(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeClaudeCode)
	lc := LaunchContext{
		Envelope:   a2a.Envelope{SystemContext: "system context", Input: "do the thing"},
		Credential: "sk-ant-secret",
		Model:      "claude-opus-5",
		WorkDir:    "/tmp/claude-work",
		MCPEndpoints: []capability.Endpoint{{
			Name:      "github-mcp",
			Transport: "stdio",
			Command:   "github-mcp-server",
			Args:      []string{"stdio"},
			EnvNames:  []string{"GITHUB_TOKEN"},
		}},
	}
	spec, err := rt.Command(lc)
	if err != nil {
		t.Fatalf("Command failed: %v", err)
	}
	if spec.Path != "claude" {
		t.Errorf("Path = %q, want claude", spec.Path)
	}
	for _, want := range []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"} {
		if !hasArg(spec.Args, want) {
			t.Errorf("args missing %q; args=%v", want, spec.Args)
		}
	}
	// Agent.spec.model overrides the default id.
	if !hasArg(spec.Args, "claude-opus-5") {
		t.Errorf("expected resolved model claude-opus-5 in args; got %v", spec.Args)
	}
	// Credential on env, never argv.
	if !hasEnv(spec.Env, "ANTHROPIC_API_KEY=sk-ant-secret") {
		t.Errorf("expected ANTHROPIC_API_KEY to carry the credential; env=%v", redact(spec.Env, "sk-ant-secret"))
	}
	for _, a := range spec.Args {
		if strings.Contains(a, "sk-ant-secret") {
			t.Errorf("credential leaked into argv %q", a)
		}
		if strings.Contains(a, "do the thing") || strings.Contains(a, "system context") {
			t.Errorf("prompt leaked into argv %q", a)
		}
	}
	// Linger suppression (ISI-4224 parity).
	if !hasEnv(spec.Env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1") {
		t.Errorf("expected CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1; env=%v", spec.Env)
	}
	// ISI-5329: no context env at all — the whole prompt rides stdin.
	for _, e := range spec.Env {
		if strings.HasPrefix(e, "KSQUAD_SYSTEM_CONTEXT") {
			t.Errorf("claude must not set context env %q (stdin channel, ISI-5329)", e)
		}
	}
	if spec.Stdin != EnvelopePrompt(lc) {
		t.Errorf("stdin must carry the full envelope prompt; got %d bytes, want %d", len(spec.Stdin), len(EnvelopePrompt(lc)))
	}
	if spec.Parse == nil {
		t.Error("Parse must be set (typed stream-json parser)")
	}
	if spec.SettleLine == nil {
		t.Error("SettleLine must be set (terminal result detector, ISI-4224)")
	}
	if spec.WorkDir != "/tmp/claude-work" {
		t.Errorf("WorkDir = %q, want /tmp/claude-work", spec.WorkDir)
	}
	// The rendered .mcp.json: server keyed by name, stdio entry, credential
	// as an env NAME reference.
	var mcpFile *WorkDirFile
	for i := range spec.WorkDirFiles {
		if spec.WorkDirFiles[i].Name == ".mcp.json" {
			mcpFile = &spec.WorkDirFiles[i]
		}
	}
	if mcpFile == nil {
		t.Fatalf("expected a rendered .mcp.json; files=%v", spec.WorkDirFiles)
	}
	var doc struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcpFile.Content, &doc); err != nil {
		t.Fatalf("rendered .mcp.json is not valid JSON: %v", err)
	}
	entry, ok := doc.MCPServers["github-mcp"]
	if !ok {
		t.Fatalf("rendered .mcp.json missing github-mcp; doc=%s", mcpFile.Content)
	}
	if entry.Type != "stdio" || entry.Command != "github-mcp-server" {
		t.Errorf("github-mcp entry = %+v, want stdio github-mcp-server", entry)
	}
	if entry.Env["GITHUB_TOKEN"] != "${GITHUB_TOKEN}" {
		t.Errorf("credential must ride as env name ref, got env=%v", entry.Env)
	}
}

// TestClaudeCommandNoMCP asserts no config file is rendered when the Run
// demands no MCP servers.
func TestClaudeCommandNoMCP(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeClaudeCode)
	spec, err := rt.Command(LaunchContext{Envelope: a2a.Envelope{Input: "hi"}})
	if err != nil {
		t.Fatalf("Command failed: %v", err)
	}
	if len(spec.WorkDirFiles) != 0 {
		t.Errorf("expected no WorkDirFiles without MCP endpoints; got %v", spec.WorkDirFiles)
	}
}

// TestClaudeCommandBYOFailsClosed pins the honest Phase-1 scope: a resolved
// BYO model endpoint is refused, not silently ignored (the CLI has no
// OpenAI-compatible lane).
func TestClaudeCommandBYOFailsClosed(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeClaudeCode)
	_, err := rt.Command(LaunchContext{
		Envelope:   a2a.Envelope{Input: "hi"},
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "llama3.1"},
	})
	if err == nil {
		t.Fatal("expected Command to fail closed on a BYO model endpoint")
	}
}

// TestClaudeSettleLine pins the ISI-4224 settle detector: only the terminal
// result event arms the runner's quiet window.
func TestClaudeSettleLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`{"type":"result","subtype":"success","is_error":false,"result":"done"}`, true},
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}`, true},
		{`{"type":"system","subtype":"init","session_id":"s1"}`, false},
		{`{"type":"assistant","message":{"role":"assistant","content":[]}}`, false},
		{`{"type":"user","message":{"role":"user","content":[]}}`, false},
		{`not json at all`, false},
	}
	for _, c := range cases {
		if got := claudeSettleLine(c.line); got != c.want {
			t.Errorf("claudeSettleLine(%s) = %v, want %v", c.line, got, c.want)
		}
	}
}

// TestClaudeParserStream walks a full print-mode stream (init, assistant text,
// Bash tool_use with command head enrichment, MCP tool_use with name
// splitting, pairing tool_results ok+error, terminal result) through the
// stateful parser and asserts the typed Progress it must produce.
func TestClaudeParserStream(t *testing.T) {
	parse := newClaudeParser()
	var got []Progress
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"s1","tools":["Bash","Read"],"mcp_servers":["github-mcp"]}`,
		`{"type":"assistant","message":{"id":"msg_1","model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"working on it"}]}}`,
		`{"type":"assistant","message":{"id":"msg_2","model":"claude-sonnet-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git status"}}]}}`,
		`{"type":"assistant","message":{"id":"msg_3","model":"claude-sonnet-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"mcp__github-mcp__create_issue","input":{"title":"x"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"clean tree","is_error":false}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"quota exceeded","is_error":true}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":4200,"num_turns":2,"result":"all done","total_cost_usd":0.05,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":200}}`,
	} {
		got = append(got, parse(line)...)
	}

	// One agent message from the assistant text turn.
	var msgs int
	for _, p := range got {
		if p.Kind == a2a.EventMessage && p.Message != nil && p.Message.Text == "working on it" {
			msgs++
		}
	}
	if msgs != 1 {
		t.Errorf("expected exactly 1 'working on it' message, got %d", msgs)
	}

	// Tool frames: two starts + two results, with Bash head enrichment and
	// MCP name/server splitting.
	tools := map[string][]*a2a.ToolPayload{}
	for _, p := range got {
		if p.Kind != a2a.EventTool || p.Tool == nil {
			continue
		}
		tools[p.Tool.Phase] = append(tools[p.Tool.Phase], p.Tool)
	}
	if len(tools["start"]) != 2 || len(tools["result"]) != 2 {
		t.Fatalf("expected 2 tool starts and 2 results; got %+v", tools)
	}
	var bashStart, mcpStart *a2a.ToolPayload
	for _, tp := range tools["start"] {
		switch tp.Name {
		case "Bash":
			bashStart = tp
		case "create_issue":
			mcpStart = tp
		}
	}
	if bashStart == nil {
		t.Fatal("missing Bash tool start frame")
	}
	if bashStart.Command != "git" {
		t.Errorf("Bash start Command = %q, want 'git' (ISI-4720 head enrichment)", bashStart.Command)
	}
	if mcpStart == nil {
		t.Fatalf("missing MCP tool start frame (split name); starts=%+v", tools["start"])
	}
	if mcpStart.Server != "github-mcp" {
		t.Errorf("MCP start Server = %q, want github-mcp", mcpStart.Server)
	}
	var bashRes, mcpRes *a2a.ToolPayload
	for _, tp := range tools["result"] {
		switch tp.Name {
		case "Bash":
			bashRes = tp
		case "create_issue":
			mcpRes = tp
		}
	}
	if bashRes == nil || bashRes.OK == nil || !*bashRes.OK {
		t.Errorf("Bash result must be OK; got %+v", bashRes)
	}
	if mcpRes == nil || mcpRes.OK == nil || *mcpRes.OK {
		t.Errorf("MCP result must be failed; got %+v", mcpRes)
	}
	if mcpRes != nil && !strings.Contains(mcpRes.Summary, "quota exceeded") {
		t.Errorf("MCP result Summary must carry the failure reason; got %q", mcpRes.Summary)
	}
	if mcpRes != nil && mcpRes.Server != "github-mcp" {
		t.Errorf("MCP result Server = %q, want github-mcp (id-bridged)", mcpRes.Server)
	}

	// The terminal result maps to one run-total usage event.
	var usage *a2a.UsagePayload
	for _, p := range got {
		if p.Kind == a2a.EventUsage && p.Usage != nil {
			if usage != nil {
				t.Fatal("expected exactly one EventUsage from the result event")
			}
			usage = p.Usage
		}
	}
	if usage == nil {
		t.Fatal("missing the result usage event")
	}
	if usage.Input != 100 || usage.Output != 50 || usage.CacheRead != 200 || usage.CacheWrite != 10 {
		t.Errorf("usage token fields wrong: %+v", usage)
	}
	if usage.CostUSD != 0.05 || usage.DurationMS != 4200 {
		t.Errorf("usage cost/duration wrong: %+v", usage)
	}
	if usage.Model != "claude-sonnet-5" || usage.ResponseModel != "claude-sonnet-5" {
		t.Errorf("usage model must come from the last assistant message: %+v", usage)
	}
	if usage.Provider != "anthropic" || usage.FinishReason != "stop" {
		t.Errorf("usage provider/finish wrong: %+v", usage)
	}
	if usage.Error != "" {
		t.Errorf("successful run usage must carry no error: %+v", usage)
	}
}

// TestClaudeParserNonJSONDegrades: a non-JSON stdout line becomes a message
// event, never an error (parse failure must not kill a running task).
func TestClaudeParserNonJSONDegrades(t *testing.T) {
	parse := newClaudeParser()
	progs := parse("claude: some diagnostic line")
	if len(progs) != 1 || progs[0].Kind != a2a.EventMessage || progs[0].Message == nil {
		t.Fatalf("expected 1 message event, got %+v", progs)
	}
	if progs[0].Message.Text != "claude: some diagnostic line" {
		t.Errorf("degraded message text = %q", progs[0].Message.Text)
	}
}

// TestClaudeParserResultError pins the errored-run shape: error text as a
// message, the error stamped onto the usage payload, and the standardized
// EventRateLimited when the failure carries a throttle signature (ISI-6054
// parity).
func TestClaudeParserResultError(t *testing.T) {
	parse := newClaudeParser()
	progs := parse(`{"type":"result","subtype":"error_during_execution","is_error":true,"duration_ms":120,"result":"API Error: 429 rate limit exceeded, retry after 30s","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":5}}`)
	var sawMsg, sawRate, sawUsage bool
	for _, p := range progs {
		switch p.Kind {
		case a2a.EventMessage:
			sawMsg = true
			if !strings.Contains(p.Message.Text, "429") {
				t.Errorf("error message must carry the error text; got %q", p.Message.Text)
			}
		case a2a.EventRateLimited:
			sawRate = true
		case a2a.EventUsage:
			sawUsage = true
			if p.Usage.ErrorType != "error_during_execution" || !strings.Contains(p.Usage.Error, "429") {
				t.Errorf("usage must carry the error; got %+v", p.Usage)
			}
			if p.Usage.Input != 10 || p.Usage.Output != 5 {
				t.Errorf("errored run usage must still carry real token totals; got %+v", p.Usage)
			}
		}
	}
	if !sawMsg || !sawRate || !sawUsage {
		t.Errorf("expected message + rateLimited + usage; saw msg=%v rate=%v usage=%v; progs=%+v", sawMsg, sawRate, sawUsage, progs)
	}
}

// TestClaudeParserUnknownToolResultIgnored: a tool_result whose id the parser
// never saw (resume shapes) reports nothing rather than an unnamed frame.
func TestClaudeParserUnknownToolResultIgnored(t *testing.T) {
	parse := newClaudeParser()
	progs := parse(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_ghost","content":"late","is_error":false}]}}`)
	if len(progs) != 0 {
		t.Errorf("expected no events for an unknown tool_use_id; got %+v", progs)
	}
}

// TestSplitClaudeMCPTool pins the mcp__<server>__<tool> splitting, including
// the malformed and non-MCP rejections.
func TestSplitClaudeMCPTool(t *testing.T) {
	cases := []struct {
		name         string
		server, tool string
		ok           bool
	}{
		{"mcp__github-mcp__create_issue", "github-mcp", "create_issue", true},
		{"mcp__ksquad-memory__search", "ksquad-memory", "search", true},
		{"Bash", "", "", false},
		{"mcp__noserver", "", "", false},
		{"mcp__server__", "", "", false},
		{"mcp____tool", "", "", false}, // empty server segment
	}
	for _, c := range cases {
		s, tool, ok := splitClaudeMCPTool(c.name)
		if ok != c.ok || (ok && (s != c.server || tool != c.tool)) {
			t.Errorf("splitClaudeMCPTool(%q) = (%q,%q,%v), want (%q,%q,%v)", c.name, s, tool, ok, c.server, c.tool, c.ok)
		}
	}
}
