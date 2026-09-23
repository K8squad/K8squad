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

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
)

// Single-sourced Codex constants (arch ISI-3646 D5): the pinned CLI revision
// and the default model + its context-window budget authority live here once,
// consumed by CLIVersion()/DefaultModel() and asserted by the unit tests.
const (
	// codexCLIVersion is the pinned official Rust `codex` revision the adapter
	// targets (ADR-017 reproducibility; ISI-3646 research).
	codexCLIVersion = "rust-v0.152.0"
	// codexDefaultModel is the runtime's default model id (D5). Agent.spec.model
	// overrides the id, not the window.
	codexDefaultModel = "gpt-5.4-codex"
	// codexContextWindow is codexDefaultModel's context-window budget authority
	// for the context Assembler (spec §6.2).
	codexContextWindow = 272000
)

// codex is the ChatGPT Codex shim adapter (epic ISI-3647, arch ISI-3646).
// Codex is OpenAI's official Rust coding agent; it speaks the OpenAI wire
// natively, so the per-user credential maps onto OPENAI_API_KEY (ShapeAPIKey,
// D1). ExecSpec has no Stdin seam, so Command targets the `ksquad-codex-exec`
// wrapper (D4), which pipes the env-transported context envelope into
// `codex exec -`. It is a conformant, first-class runtime — not experimental.
type codex struct{}

func (codex) Type() string                     { return apiv1alpha1.RuntimeTypeCodex }
func (codex) CLIVersion() string               { return codexCLIVersion }
func (codex) CredentialShape() CredentialShape { return ShapeAPIKey }

func (codex) Capabilities() a2a.Capabilities {
	return a2a.Capabilities{
		Streaming:         true,
		ToolCalls:         true,
		InteractivePrompt: true,
		BYOModelEndpoint:  true, // OpenAI-compatible base URL / model_providers (D6)
		ArtifactKinds:     []string{"file", "patch"},
		Docker:            true,
		GitHub:            true,
		PackageInstall:    true,
	}
}

func (codex) DefaultModel() a2a.ModelInfo {
	return a2a.ModelInfo{ID: codexDefaultModel, ContextWindow: codexContextWindow}
}

func (r codex) Command(lc LaunchContext) (ExecSpec, error) {
	// Envelope rides env (never argv) so the prompt stays out of the process
	// table; the ksquad-codex-exec wrapper pipes it into `codex exec -` (D4).
	env := envelopeEnv(lc)
	// Codex speaks the OpenAI wire natively, so the per-user credential AND a
	// BYO model route (story 5.7) both map onto OPENAI_API_KEY. Emit exactly
	// one: a BYO endpoint carries its own token+base URL via modelRouteEnv;
	// otherwise the per-user credential is the OpenAI key. Emitting both would
	// shadow the route token (glibc getenv is first-wins).
	if route := modelRouteEnv(lc.ModelRoute); len(route) > 0 {
		env = append(env, route...)
	} else if lc.Credential != "" {
		env = append(env, "OPENAI_API_KEY="+lc.Credential)
	}
	// CODEX_HOME points the CLI at its config dir — the sandbox workdir where
	// the rendered config.toml (mcp_servers) is materialized below.
	env = append(env, "CODEX_HOME="+lc.WorkDir)

	spec := ExecSpec{
		Path: "ksquad-codex-exec",
		Args: []string{
			"--json",
			"--skip-git-repo-check",
			"-m", resolveModel(r, lc),
			"-C", lc.WorkDir,
			"-s", "workspace-write",
			"-a", "never",
		},
		Env:     env,
		WorkDir: lc.WorkDir,
		// ISI-4732: codex `exec --json` emits a JSONL thread-event stream on
		// stdout (thread.started / turn.started / item.completed / turn.completed
		// / error — ISI-3644 research §2). Decode it into typed Progress so tool
		// and llm.call activity reaches the A2A wire and the Epic D telemetry
		// spine as EventTool/EventUsage instead of the runner's opaque
		// line→message fallback (which emitted ZERO gen_ai.tool.call / llm.call
		// spans for codex runs — the residual gap ISI-4720 left for the
		// non-opencode runtimes). Stateless: item.completed carries the whole
		// item (command + exit_code + status together), so no begin/end
		// correlation is needed.
		Parse: parseCodexLine,
	}
	// Epic C (ADR-044 step 6): codex's config.toml, rendered from the projected
	// IR at start — the [mcp_servers.*] section plus, when a BYO endpoint is set
	// (story 5.7, S6), the [model_providers.ksquad-byo] block pointing the CLI at
	// it (a safe superset of the OPENAI_BASE_URL env above). Credentials/tokens
	// ride as env inside the process, never as literals in the document.
	// A BYO endpoint alone (no MCP servers) still needs the file, so render
	// directly rather than via the MCP-only mcpWorkDirFile helper.
	if len(lc.MCPEndpoints) > 0 || lc.ModelRoute.Endpoint != "" {
		content, err := capability.RenderCodexConfig(lc.MCPEndpoints, lc.ModelRoute.Endpoint)
		if err != nil {
			return ExecSpec{}, err
		}
		spec.WorkDirFiles = append(spec.WorkDirFiles, WorkDirFile{Name: "config.toml", Content: content})
	}
	return spec, nil
}

func init() { Register(codex{}) }

// codexItem is the `item` payload of a codex thread event (item.started /
// item.updated / item.completed). One envelope carries every item flavor; each
// kind populates its own fields. The discriminant is Type ("command_execution",
// "agent_message", "mcp_tool_call", "web_search", "reasoning", "error", …),
// distinct from the OUTER event type ("item.completed") one nesting level up.
type codexItem struct {
	Type string `json:"type"`
	// agent_message / reasoning / error text.
	Text    string `json:"text"`
	Message string `json:"message"`
	// command_execution: the shell command (a string like `bash -lc "git …"`),
	// its terminal status and exit code. Command rides as RawMessage so a
	// string-vs-array wire drift can never fail the whole-line decode.
	Command  json.RawMessage `json:"command"`
	Status   string          `json:"status"`
	ExitCode *int            `json:"exit_code"`
	// mcp_tool_call: the MCP server + tool that was invoked, plus its raw args.
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	// web_search: the query the model issued.
	Query string `json:"query"`
}

// codexUsage is the token block on a turn.completed event (codex reports one
// per completed turn; the totals are summed across turns by the run's usage
// views). input_tokens is the full input count and cached_input_tokens the
// cached subset of it (carried distinctly onto CacheRead, ISI-4238).
type codexUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	ReasoningTokens   int `json:"reasoning_output_tokens"`
}

// parseCodexLine decodes one codex `exec --json` stdout line into typed
// Progress events (ISI-4732). The thread-event shapes (codex rust-v0.152.0,
// ISI-3644 research §2):
//
//	{"type":"item.completed","item":{"type":"command_execution",
//	  "command":"bash -lc \"git status\"","exit_code":0,"status":"completed"}}
//	{"type":"item.completed","item":{"type":"mcp_tool_call",
//	  "server":"github","tool":"list_issues","status":"completed"}}
//	{"type":"item.completed","item":{"type":"agent_message","text":"…"}}
//	{"type":"turn.completed","usage":{"input_tokens":2606,
//	  "cached_input_tokens":2048,"output_tokens":54}}
//	{"type":"error","message":"…"}
//
// Only item.completed settles a call — item.started/updated are progress
// bookkeeping (dropped) so exactly one span settles per call, the same
// one-event-per-call posture the opencode adapter relies on. A non-JSON line
// (codex diagnostics can interleave on stdout) degrades to a message event,
// never an error: parse failure must not kill a running task.
func parseCodexLine(line string) []Progress {
	var ev struct {
		Type  string      `json:"type"`
		Item  *codexItem  `json:"item"`
		Usage *codexUsage `json:"usage"`
		// Top-level error/turn.failed message.
		Message string `json:"message"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: line, Trust: "untrusted"},
		}}
	}
	switch ev.Type {
	case "item.completed":
		if ev.Item == nil {
			return nil
		}
		return progressFromCodexItem(ev.Item)
	case "turn.completed":
		if u := usageFromCodexTurn(ev.Usage); u != nil {
			return []Progress{{Kind: a2a.EventUsage, Usage: u}}
		}
		return nil
	case "turn.failed", "error":
		msg := ev.Message
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		if msg == "" {
			msg = "codex error"
		}
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: msg, Trust: "untrusted"},
		}}
	default: // thread.started, turn.started, item.started/updated, future shapes
		return nil
	}
}

// progressFromCodexItem maps one completed thread item onto its typed Progress.
// command_execution and mcp_tool_call become a single phase="result" EventTool
// (the telemetry spine settles one span from an orphan result, ISI-4238);
// agent_message becomes a message; reasoning/file_change/todo_list are dropped
// as bookkeeping.
func progressFromCodexItem(it *codexItem) []Progress {
	switch it.Type {
	case "command_execution":
		// codex models every shell call as the single command_execution tool, so
		// name it "bash" (the telemetry spine categorizes bash→ksquad.tool.type
		// "bash"); the recognized executable head (git/kubectl/…) is extracted
		// IN-PROCESS from the command, before args are hashed, so a bash-wrapped
		// git call is categorized by what it RAN, not left an opaque shell span
		// (ISI-4720 reuse). Only the bounded head token travels, never the line.
		cmd := codexCommandString(it.Command)
		tool := &a2a.ToolPayload{Name: "bash", Phase: "result"}
		if head := codexCommandHead(cmd); head != "" {
			tool.Command = head
		}
		tool.OK = codexCommandOK(it)
		return []Progress{{Kind: a2a.EventTool, Tool: tool, ToolArgs: cmd}}
	case "mcp_tool_call":
		if it.Tool == "" {
			return nil
		}
		// A set Server makes the telemetry mapping emit an mcp.call span (and
		// observe the MCP duration histogram) instead of gen_ai.tool.call.
		tool := &a2a.ToolPayload{Name: it.Tool, Server: it.Server, Phase: "result"}
		tool.OK = codexStatusOK(it.Status)
		var args string
		if len(it.Arguments) > 0 {
			args = string(it.Arguments)
		}
		return []Progress{{Kind: a2a.EventTool, Tool: tool, ToolArgs: args}}
	case "web_search":
		ok := true
		tool := &a2a.ToolPayload{Name: "web_search", Phase: "result", OK: &ok}
		return []Progress{{Kind: a2a.EventTool, Tool: tool, ToolArgs: it.Query}}
	case "agent_message":
		if it.Text == "" {
			return nil
		}
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: it.Text, Trust: "untrusted"},
		}}
	case "error":
		msg := it.Message
		if msg == "" {
			msg = it.Text
		}
		if msg == "" {
			return nil
		}
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: msg, Trust: "untrusted"},
		}}
	default: // reasoning, file_change, todo_list, future shapes: bookkeeping
		return nil
	}
}

// codexCommandString reads a command_execution's command, tolerating both the
// v0.152.0 string form (`"bash -lc \"git status\""`) and an argv-array form
// (`["bash","-lc","git status"]`) so a wire drift degrades to a plain shell span
// rather than failing the line. An unreadable shape yields "".
func codexCommandString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err == nil {
		return strings.Join(argv, " ")
	}
	return ""
}

// codexShellWrappers are the POSIX-shell invocation prefixes codex wraps a
// command in (`bash -lc "<script>"`). Peeling one exposes the real executable
// head (git/kubectl/…) to a2a.ShellCommandHead instead of surfacing "bash".
var codexShellWrappers = []string{
	"bash -lc ", "bash -c ", "bash -lic ",
	"sh -lc ", "sh -c ", "sh -lic ",
	"zsh -lc ", "zsh -c ",
}

// codexCommandHead extracts the recognized executable head from a codex shell
// command (ISI-4720 reuse). codex wraps commands as `bash -lc "<script>"`, so it
// first peels a known shell wrapper (and the surrounding quotes on the script)
// and then runs a2a.ShellCommandHead on the remainder — returning the head ONLY
// when it is a recognized tool, never an arbitrary argument.
func codexCommandHead(command string) string {
	s := strings.TrimSpace(command)
	for _, pfx := range codexShellWrappers {
		if strings.HasPrefix(s, pfx) {
			s = strings.TrimSpace(s[len(pfx):])
			s = strings.Trim(s, "\"'")
			break
		}
	}
	return a2a.ShellCommandHead(s)
}

// codexCommandOK derives a command_execution's tri-state outcome: a reported
// exit_code is authoritative (0 = ok), otherwise the terminal status decides,
// otherwise unknown (nil) — never guessed (D1 AC).
func codexCommandOK(it *codexItem) *bool {
	if it.ExitCode != nil {
		ok := *it.ExitCode == 0
		return &ok
	}
	return codexStatusOK(it.Status)
}

// codexStatusOK maps a codex item terminal status onto the tri-state tool
// outcome: "completed" = ok, "failed"/"error" = not ok, anything else (in
// flight, absent) = unknown (nil).
func codexStatusOK(status string) *bool {
	switch status {
	case "completed":
		ok := true
		return &ok
	case "failed", "error":
		ok := false
		return &ok
	default:
		return nil
	}
}

// usageFromCodexTurn maps a turn.completed usage block onto an EventUsage
// payload (ISI-4238). Returns nil when the turn carried no usage — a bare
// turn.completed stays dropped rather than emitting zeroed usage that would
// poison the run's token totals. Model is left empty; the engine backfills the
// launch model before the event leaves the process (pkg/shim/engine.go), the
// same contract opencode's step_finish relies on.
func usageFromCodexTurn(u *codexUsage) *a2a.UsagePayload {
	if u == nil {
		return nil
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.ReasoningTokens == 0 && u.CachedInputTokens == 0 {
		return nil
	}
	return &a2a.UsagePayload{
		Input:     u.InputTokens,
		Output:    u.OutputTokens,
		Reasoning: u.ReasoningTokens,
		CacheRead: u.CachedInputTokens,
	}
}
