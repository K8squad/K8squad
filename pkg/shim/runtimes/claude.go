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
	"fmt"
	"strings"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
)

// claudeCode is the Claude Code shim adapter (ISI-6697 Phase 1 / WS-1,
// ISI-6700). Headless, Claude Code runs `claude -p` print mode whose
// `--output-format stream-json` wire is one JSON event per line (system /
// assistant / user / result) — the same event-per-line contract opencode's
// `--format=json` uses, so this adapter mirrors the opencode ExecSpec shape
// exactly: typed Parse, a SettleLine on the terminal event, tool/MCP
// passthrough onto the A2A wire, and the whole prompt on stdin.
//
// Phase 1 is the Anthropic-API-key lane only (ISI-6697 plan §4): the stored
// secret maps onto ANTHROPIC_API_KEY. The subscription lane
// (CLAUDE_CODE_OAUTH_TOKEN, ShapeOAuthToken) is Phase 2, gated on the board
// ToS decision (ISI-3661).
type claudeCode struct{}

func (claudeCode) Type() string { return apiv1alpha1.RuntimeTypeClaudeCode }

// CLIVersion pins the Claude Code release baked into the ksquad-shim-claude
// image (Dockerfile.shim cli-claude stage, CLAUDE_VERSION — keep the two in
// lockstep, ADR-017). The stage installs Anthropic's native musl binary as
// `claude` and asserts the version at build time.
func (claudeCode) CLIVersion() string               { return "2.1.236" }
func (claudeCode) CredentialShape() CredentialShape { return ShapeAPIKey }

// Capabilities is the honest card (story 5.2, F15):
//   - InteractivePrompt=false: print mode is one-shot — there is no
//     input-required turn, so a Run that needs one dead-ends rather than
//     prompting (the same first-class gap hermes advertises).
//   - BYOModelEndpoint=false: the CLI rides Anthropic's own wire; there is no
//     OpenAI-compatible BYO lane in Phase 1 — and Command fails closed if a
//     model route is ever resolved for this runtime.
//   - RateLimitSignal=true: the terminal result parser emits the
//     standardized §4 EventRateLimited on provider throttles
//     (signature-based over the error text, the opencode ISI-6054 pattern).
func (claudeCode) Capabilities() a2a.Capabilities {
	return a2a.Capabilities{
		Streaming:         true,
		ToolCalls:         true,
		InteractivePrompt: false,
		BYOModelEndpoint:  false,
		RateLimitSignal:   true,
		ArtifactKinds:     []string{"file", "patch"},
		Docker:            true,
		GitHub:            true,
		PackageInstall:    true, // no private package sandbox — installs ride Bash directly
	}
}

// DefaultModel is the curated default, consistent with the console's curated
// Claude family (console/lib/modelHints.ts: claude-sonnet-5 "balanced" — the
// implementer/manager preset default in console/lib/presets.ts), NOT freeform.
// The window is the Claude family standard context budget the shim set
// already uses for the sonnet tier (openCode.DefaultModel). Agent.spec.model
// overrides the id, not the window (spec §6.2).
func (claudeCode) DefaultModel() a2a.ModelInfo {
	return a2a.ModelInfo{ID: "claude-sonnet-5", ContextWindow: 200000}
}

func (r claudeCode) Command(lc LaunchContext) (ExecSpec, error) {
	// Fail closed on a BYO model route (Phase-1 scope, plan §4): the CLI
	// speaks the Anthropic wire only, so silently mapping a route onto
	// OPENAI_* env would be a no-op at best and a misdirected run at worst —
	// refuse the launch honestly instead.
	if lc.ModelRoute.Endpoint != "" {
		return ExecSpec{}, fmt.Errorf("claude-code: BYO model endpoint unsupported (rides the Anthropic wire; ISI-6697 Phase 1 plan §4)")
	}
	var env []string
	if lc.Credential != "" {
		env = append(env, "ANTHROPIC_API_KEY="+lc.Credential)
	}
	// ISI-4224 parity: print mode's event loop can be held open by the CLI's
	// non-essential background traffic (update check, telemetry, error
	// reporting) after the result event was printed; suppress them at the
	// source so process exit stays the norm and SettleLine is the backstop.
	env = append(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	spec := ExecSpec{
		Path: "claude",
		Args: []string{
			"-p", // print mode: one-shot headless run (the ISI-2112 spike GO contract)
			"--output-format", "stream-json",
			"--verbose", // the full event stream (assistant/user tool turns) requires verbose in print mode
			"--model", resolveModel(r, lc),
			// The Kata sandbox IS the isolation boundary (per-Run PVC, no
			// host surface), so autonomous tool execution is the honest
			// posture: without a bypass print mode DENIES every
			// permission-gated tool and a Run cannot act at all.
			"--dangerously-skip-permissions",
		},
		Env: env,
		// ISI-5329 parity with opencode: the whole prompt (system context +
		// work instruction) rides stdin — `claude -p` reads its prompt from
		// stdin when no positional is given — so the token-budgeted context
		// never touches an env string (E2BIG, ISI-5328) or argv
		// (NFR-SEC1, the process table).
		Stdin:      EnvelopePrompt(lc),
		Parse:      newClaudeParser(),
		WorkDir:    lc.WorkDir,
		SettleLine: claudeSettleLine,
	}
	// Epic C (ADR-044 step 6): Claude Code discovers project-scope MCP
	// servers from a .mcp.json in its working directory — render the
	// projected IR with the existing claude-code renderer (credentials as
	// ${ENV} names, never literal; tool filters as x-ksquad extension keys).
	if f, err := mcpWorkDirFile(".mcp.json", capability.RenderClaudeCode, lc.MCPEndpoints); err != nil {
		return ExecSpec{}, err
	} else if f != nil {
		spec.WorkDirFiles = append(spec.WorkDirFiles, *f)
	}
	return spec, nil
}

func init() { Register(claudeCode{}) }

// claudeSettleEvent is the claude stream-json line type the ISI-4224 settle
// detector matches: print mode emits exactly one {"type":"result",…}
// terminal event per run (subtype success | error_max_turns |
// error_during_execution) — the last observable "the session's work is done"
// marker on stdout, the same role opencode's step_finish plays.
const claudeSettleEvent = "result"

// claudeSettleLine reports whether one claude stream-json stdout line is the
// run-terminal result event. Non-JSON lines and other event types (system,
// assistant, user) report false: only the terminal marker arms the runner's
// quiet window, and no earlier line re-arms it mid-flight.
func claudeSettleLine(line string) bool {
	var ev struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return false
	}
	return ev.Type == claudeSettleEvent
}

// claudeContentBlock is one content block of an assistant or user message:
// text ({"type":"text","text":…}), tool_use
// ({"type":"tool_use","id":"toolu_…","name":"Bash","input":{…}}), or
// tool_result ({"type":"tool_result","tool_use_id":"toolu_…","content":…,
// "is_error":bool}).
type claudeContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// tool_use fields
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result fields
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // plain string OR [{type:"text",…}]
	IsError   bool            `json:"is_error"`
}

// claudeMessage is the Anthropic API message riding an assistant or user
// stream event ({"type":"assistant"|"user","message":{…}}).
type claudeMessage struct {
	Model   string               `json:"model"`
	Role    string               `json:"role"`
	Content []claudeContentBlock `json:"content"`
}

// claudeUsage is the token block on the terminal result event (assistant
// messages carry one per turn too — deliberately NOT emitted per-message, so
// the run's token totals count the result event's run-wide record exactly
// once).
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// claudeEvent is the union stream-json envelope: system/init bookkeeping,
// assistant/user messages, and the terminal result
// ({"type":"result","subtype":…,"is_error":…,"duration_ms":…,"result":…,
// "total_cost_usd":…,"usage":{…}}).
type claudeEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Message json.RawMessage `json:"message"`

	// result-event fields
	IsError      bool         `json:"is_error"`
	DurationMS   int64        `json:"duration_ms"`
	Result       string       `json:"result"`
	TotalCostUSD float64      `json:"total_cost_usd"`
	Usage        *claudeUsage `json:"usage"`
}

// claudeToolRef remembers what a tool_result's tool_use_id refers to (the
// bare, MCP-split tool name and server): the user tool_result block
// references the call by id only — the name lives on the originating
// assistant tool_use block.
type claudeToolRef struct {
	Name   string
	Server string
}

// newClaudeParser returns a per-run stateful stdout parser (the
// newOpenCodeParser pattern): it bridges tool_use ids to names across the
// assistant→user event pair and remembers the last-reported assistant model
// so the terminal result's usage event can carry it (the result event itself
// reports no model). Command builds a fresh ExecSpec — hence a fresh parser —
// per Run, so no cross-run leakage is possible.
func newClaudeParser() func(line string) []Progress {
	tools := map[string]claudeToolRef{}
	lastModel := ""
	return func(line string) []Progress {
		return parseClaudeLine(line, tools, &lastModel)
	}
}

// parseClaudeLine decodes one stream-json stdout line into typed Progress
// (ISI-4188 gap 7 parity): assistant text becomes an EventMessage, tool_use
// and tool_result become EventTool start/result frames, and the terminal
// result becomes the run-total EventUsage. A non-JSON line (the CLI's own
// diagnostics interleaved on stdout) degrades to a message event, never an
// error: parse failure must not kill a running task.
func parseClaudeLine(line string, tools map[string]claudeToolRef, lastModel *string) []Progress {
	var ev claudeEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: line, Trust: "untrusted"},
		}}
	}
	switch ev.Type {
	case "assistant":
		var msg claudeMessage
		if err := json.Unmarshal(ev.Message, &msg); err != nil {
			return nil
		}
		if msg.Model != "" {
			*lastModel = msg.Model
		}
		return claudeAssistantProgress(msg, tools)
	case "user":
		var msg claudeMessage
		if err := json.Unmarshal(ev.Message, &msg); err != nil {
			return nil
		}
		return claudeToolResults(msg, tools)
	case "result":
		return claudeResultProgress(ev, *lastModel)
	default: // system/init and future shapes: bookkeeping, not wire events
		return nil
	}
}

// claudeAssistantProgress maps one assistant message onto Progress: text
// blocks become EventMessage; tool_use blocks become phase="start" EventTool
// frames with the raw input as ToolArgs (hashed onto Tool.ArgsSHA256 by the
// engine funnel before it goes anywhere) and the id remembered so the pairing
// tool_result can name the tool again.
func claudeAssistantProgress(msg claudeMessage, tools map[string]claudeToolRef) []Progress {
	var out []Progress
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			out = append(out, Progress{
				Kind:    a2a.EventMessage,
				Message: &a2a.MessagePayload{Role: "agent", Text: b.Text, Trust: "untrusted"},
			})
		case "tool_use":
			tool := &a2a.ToolPayload{Name: b.Name, Phase: "start"}
			// MCP passthrough: Claude Code names an MCP-served tool
			// mcp__<server>__<tool>; split it so the telemetry spine buckets
			// the call as an mcp.call with server + bare tool (the codex
			// wire's shape — opencode needs longest-prefix matching because
			// its flattening is ambiguous; claude's dedicated joiner is not).
			if server, bare, ok := splitClaudeMCPTool(b.Name); ok {
				tool.Server = server
				tool.Name = bare
			}
			// ISI-4720 parity: Claude Code models shell work as arguments to
			// its "Bash" tool; surface the invoked executable's bounded head
			// token so the span categorizes by what actually ran.
			if b.Name == "Bash" || isShellTool(b.Name) {
				if head := a2a.ShellCommandHead(bashCommandArg(b.Input)); head != "" {
					tool.Command = head
				}
			}
			if b.ID != "" {
				tools[b.ID] = claudeToolRef{Name: tool.Name, Server: tool.Server}
			}
			var args string
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			out = append(out, Progress{Kind: a2a.EventTool, Tool: tool, ToolArgs: args})
		}
	}
	return out
}

// claudeToolResultText extracts the readable text of a tool_result content:
// the wire carries either a plain string or an array of content blocks — the
// first text block wins (bounded use only: the error-summary normalizer
// truncates).
func claudeToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []claudeContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// claudeToolResults maps one user message's tool_result blocks onto
// phase="result" EventTool frames: OK inverts is_error, and the error summary
// carries the failure reason (ISI-5211 parity). Name and server come from the
// remembered tool_use id — the result block itself carries neither. A result
// for an id this parser never saw (resume shapes) reports nothing rather
// than an unnamed frame.
func claudeToolResults(msg claudeMessage, tools map[string]claudeToolRef) []Progress {
	var out []Progress
	for _, b := range msg.Content {
		if b.Type != "tool_result" {
			continue
		}
		ref, ok := tools[b.ToolUseID]
		if !ok {
			continue
		}
		delete(tools, b.ToolUseID) // ids are single-use; bound the map in long runs
		tool := &a2a.ToolPayload{Name: ref.Name, Server: ref.Server, Phase: "result"}
		succeeded := !b.IsError
		tool.OK = &succeeded
		if b.IsError {
			tool.Summary = a2a.ToolErrorSummary(claudeToolResultText(b.Content))
		}
		out = append(out, Progress{Kind: a2a.EventTool, Tool: tool})
	}
	return out
}

// claudeResultProgress maps the terminal result event onto Progress (the
// opencode step_finish→usage parity): the run-total token/cost/duration
// record becomes one EventUsage. An errored run additionally surfaces the
// error text as a message and on the usage payload, plus the standardized
// EventRateLimited when the failure text carries a provider throttle
// signature (ISI-6054 parity — claude surfaces 429s as error text, not a
// status field).
func claudeResultProgress(ev claudeEvent, lastModel string) []Progress {
	errType := ev.Subtype
	if errType == "" {
		errType = "error"
	}
	text := ev.Result
	if text == "" && ev.IsError {
		text = "claude code error: " + errType
	}
	var out []Progress
	if ev.IsError {
		out = append(out, Progress{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: text, Trust: "untrusted"},
		})
	}
	if ev.Usage == nil {
		if !ev.IsError {
			return nil // a bare result without usage stays dropped — wire drift degrades silently
		}
		out = append(out, Progress{
			Kind:  a2a.EventUsage,
			Usage: &a2a.UsagePayload{ErrorType: errType, Error: text},
		})
	} else {
		// The result event carries no model id; the last assistant message's
		// model is the run's served model (requested == served for a
		// single-model runtime — the opencode equivalence note). Empty when
		// the run errored before any assistant turn; the engine backfills
		// from the launch model (pkg/shim/engine.go).
		u := &a2a.UsagePayload{
			Input:         ev.Usage.InputTokens,
			Output:        ev.Usage.OutputTokens,
			CacheRead:     ev.Usage.CacheReadInputTokens,
			CacheWrite:    ev.Usage.CacheCreationInputTokens,
			CostUSD:       ev.TotalCostUSD,
			DurationMS:    ev.DurationMS,
			Model:         lastModel,
			ResponseModel: lastModel,
			Provider:      "anthropic",
			FinishReason:  claudeFinishReason(ev.Subtype),
		}
		if ev.IsError {
			u.ErrorType = errType
			u.Error = text
		}
		out = append(out, Progress{Kind: a2a.EventUsage, Usage: u})
	}
	if ev.IsError && isRateLimitError(errType, text) {
		out = append(out, Progress{
			Kind:        a2a.EventRateLimited,
			RateLimited: &a2a.RateLimitedPayload{},
		})
	}
	return out
}

// claudeFinishReason maps the result subtype onto
// gen_ai.response.finish_reasons (GH #635 parity): a successful run is the
// canonical "stop"; the error subtypes pass through as the wire's own tokens
// (error_max_turns is the turn-budget analogue of "length", but renaming it
// would lose the native signal).
func claudeFinishReason(subtype string) string {
	if subtype == "success" || subtype == "" {
		return "stop"
	}
	return subtype
}

// splitClaudeMCPTool splits a Claude Code MCP tool name
// (`mcp__<server>__<tool>`) into its server and bare tool. Unlike opencode's
// flattened `<server>_<tool>` (ambiguous, hence longest-prefix matching),
// claude's dedicated double-underscore joiner is unambiguous. ok=false when
// the name is not MCP-served (a built-in tool like Bash/Edit/Read) or
// malformed.
func splitClaudeMCPTool(name string) (server, bareTool string, ok bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(name, prefix)
	i := strings.Index(rest, "__")
	if i <= 0 || i+2 >= len(rest) {
		return "", "", false
	}
	return rest[:i], rest[i+2:], true
}
