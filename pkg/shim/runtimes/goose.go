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
	"path/filepath"
	"strings"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
)

// gooseRuntime is the goose shim engine (ISI-6128 Phase B host side, ISI-6200),
// per the locked spike decision (mesh docs/isi-6128-goose-integration-spike.md
// @ 2b9a1ed; raw probe artifacts in ~/.paperclip-spike/isi-6128/ on claude-dev).
// Phase B integrates `goose run` one-shot per Run with --output-format
// stream-json — NOT ACP (`goose acp`/`goose serve` proven in the spike,
// deliberately deferred). The engine mirrors the codex/opencode split: this
// file is the k8squad host side; mesh NewGooseAdapter (goose flavor over
// ChildProcessAdapter, ISI-6197) is the mesh side.
type gooseRuntime struct{}

func (gooseRuntime) Type() string                     { return apiv1alpha1.RuntimeTypeGoose }
func (gooseRuntime) CredentialShape() CredentialShape { return ShapeAPIKey }

// CLIVersion pins the aaif-goose/goose release the engine targets (spike
// decision: repo moved block/goose → aaif-goose/goose; v1.55.0, 2026-10-16).
// The static musl build (goose-x86_64-unknown-linux-musl) is what the Kata
// sandbox image pins — keep image and this constant in lockstep (ADR-017).
func (gooseRuntime) CLIVersion() string { return "v1.55.0" }

func (gooseRuntime) Capabilities() a2a.Capabilities {
	return a2a.Capabilities{
		// stream-json emits per-token message deltas — Parse coalesces them
		// into typed message progress (spike §1, verified live).
		Streaming: true,
		// ToolCalls is proven, not assumed: a forced tool loop on the pinned
		// binary (developer builtin, `shell` tool) captured toolRequest /
		// toolResponse content blocks on the wire, correlated by id and
		// carrying the serving extension name (_meta.goose_extension).
		ToolCalls: true,
		// goose run -i - is a headless one-shot; interactivity is ACP-side
		// (deferred by the spike decision).
		InteractivePrompt: false,
		BYOModelEndpoint:  true,
		// NOT advertised yet: goose v1.55.0's error frame shape is unverified
		// (the probes captured no failing run). newGooseParser maps a defensive
		// error frame onto EventRateLimited using the shared opencode
		// signature matcher, but peers must not rely on the signal until a
		// live 429 capture pins the shape — then flip this flag.
		RateLimitSignal: false,
		ArtifactKinds:   []string{"file", "patch"},
		Docker:          false,
		GitHub:          false,
		// goose owns its developer-tools sandbox; the shim does not install
		// packages for it.
		PackageInstall: false,
	}
}

// DefaultModel mirrors the opencode declaration (claude-sonnet-4 @ 200K):
// the scheduler pre-resolves the model (spec §6.2, Agent.spec.model /
// ModelRoute.Model both take precedence), so this is only the last-resort
// fallback — goose's provider zoo (openai/anthropic/ollama/…) makes any
// single id provisional; the sibling-consistent default keeps the honest
// context-window authority.
func (gooseRuntime) DefaultModel() a2a.ModelInfo {
	return a2a.ModelInfo{ID: "claude-sonnet-4", ContextWindow: 200000}
}

// gooseConfigRelPath is where goose reads its MCP extension config relative
// to the Run workdir: config.yaml lives at
// $XDG_CONFIG_HOME/goose/config.yaml (verified live on the pinned v1.55.0
// binary via `goose info` with XDG_CONFIG_HOME overridden), the engine
// stamps XDG_CONFIG_HOME=<WorkDir>/.config (below), so the rendered document
// materializes at this subpath of the workdir.
const gooseConfigRelPath = ".config/goose/config.yaml"

// Command builds the one-shot goose invocation per the spike decision:
//
//	goose run -i - --output-format stream-json --no-session --no-profile
//
// The prompt rides stdin (-i -), never argv — NFR-SEC1, the same invariant as
// the codex wrapper and opencode `run`. --no-session avoids session
// persistence; --no-profile skips goose's DEFAULT extension set (verified
// live: without it, v1.55.0 loads a platform extension set — analyze, apps,
// delegate, extensionmanager, … — regardless of HOME/XDG state, which would
// hand the model undeclared tool surface). On MCP Runs the flag is dropped
// (see MCPEndpoints below): v1.55.0 couples config.yaml extension loading to
// profile loading, so the rendered extensions only materialize without it —
// and the egress gate owns tool scoping for the platform set that rides
// along on those Runs.
func (r gooseRuntime) Command(lc LaunchContext) (ExecSpec, error) {
	env := []string{
		// Approval semantics: headless engine pins auto (spike env contract).
		"GOOSE_MODE=auto",
		// Mandatory sandbox hygiene (spike env contract).
		"GOOSE_TELEMETRY_OFF=1",
		"GOOSE_DISABLE_KEYRING=1",
		// State-dir isolation (spike risk item "config/session dir isolation
		// per sandbox", evidence: `goose info` on the pinned binary). v1.55.0
		// has NO GOOSE_CONFIG_DIR — goose resolves its config dir through the
		// XDG base-dir strategy, so XDG_CONFIG_HOME is the evidence-backed
		// knob, and it must match the mesh adapter's stamping (mesh/goose.go:
		// XDG_CONFIG_HOME=<launch dir>/.config) so supervisor and engine keep
		// one convention. sessions.db (XDG_DATA_HOME/goose/sessions) and the
		// logs dir (XDG_STATE_HOME/goose/logs) ride the sibling XDG roots —
		// all three are pointed inside the per-Run WorkDir so no goose state
		// (config.yaml, secrets.yaml, sessions.db, logs) leaks across Runs.
		"XDG_CONFIG_HOME=" + filepath.Join(lc.WorkDir, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(lc.WorkDir, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(lc.WorkDir, ".local", "state"),
		"GOOSE_MODEL=" + resolveModel(r, lc),
	}
	switch {
	case lc.ModelRoute.Endpoint != "":
		// BYO OpenAI-compatible endpoint (stories 5.6 + 5.7): goose's openai
		// provider + the shared OPENAI_BASE_URL/OPENAI_API_KEY mapping
		// (modelRouteEnv — the zero-credential Ollama lane rides the same pair
		// with the conventional placeholder key). This matches the mesh
		// adapter's BYO route stamping (mesh/goose.go) and the conformance
		// lane contract (conformance/verify.go checkCredentialMetadata), so
		// supervisor and engine agree on one wire. The NATIVE ollama lane
		// (GOOSE_PROVIDER=ollama + OLLAMA_HOST, no /v1 suffix — the exact pair
		// the spike probed live on v1.55.0) stays available to callers that
		// pin their own env; the engine never guesses it, because the native
		// server root cannot be reconstructed from the OpenAI-compatible
		// ModelRoute endpoint alone.
		env = append(env, append([]string{"GOOSE_PROVIDER=openai"}, modelRouteEnv(lc.ModelRoute)...)...)
	case lc.Credential != "":
		// Vendor lane: the per-user api-key credential (ShapeAPIKey) rides
		// goose's openai provider key env.
		env = append(env, "GOOSE_PROVIDER=openai", "OPENAI_API_KEY="+lc.Credential)
	default:
		// Fail closed: goose without a provider key or route wants an
		// interactive `goose configure` — never correct inside a sandbox.
		return ExecSpec{}, fmt.Errorf("goose: no credential and no model route — refusing to launch an unconfigured provider")
	}

	args := []string{
		"run",
		"-i", "-",
		"--output-format", "stream-json",
		"--no-session",
	}
	spec := ExecSpec{
		Path: "goose",
		Env:  env,
		// The whole envelope (system context + work instruction) rides stdin
		// via EnvelopePrompt — same channel as opencode `run` and the codex
		// wrapper (NFR-SEC1); goose reads the prompt from stdin with -i -.
		Stdin:      EnvelopePrompt(lc),
		WorkDir:    lc.WorkDir,
		Parse:      newGooseParser(),
		SettleLine: gooseSettleLine,
	}

	if len(lc.MCPEndpoints) == 0 {
		// Plain Run: the locked surface — --no-profile keeps goose's default
		// platform extensions (delegate/apps/extensionmanager/…) out of the
		// model's tool surface.
		spec.Args = append(args, "--no-profile")
		return spec, nil
	}

	// Epic C (ADR-044 step 6): the Run's MCP IR renders into goose's
	// config.yaml `extensions` section (capability renderer — one IR, N
	// renderers; credentials ride as env NAMES, never literal, ADR-045 D5).
	// The file materializes at gooseConfigRelPath inside WorkDir, which IS
	// the XDG_CONFIG_HOME root the engine stamps above, so `goose run` loads
	// exactly this document. v1.55.0 skips config.yaml extensions under
	// --no-profile (verified live), so MCP Runs launch without the flag —
	// see the Command doc comment for the platform-set trade-off.
	content, err := capability.RenderGooseConfig(lc.MCPEndpoints)
	if err != nil {
		return ExecSpec{}, fmt.Errorf("goose: render mcp config: %w", err)
	}
	spec.Args = args
	spec.WorkDirFiles = append(spec.WorkDirFiles, WorkDirFile{Name: gooseConfigRelPath, Content: content})
	return spec, nil
}

func init() { Register(gooseRuntime{}) }

// gooseSettleEvent is the goose stream-json terminal marker (ISI-4224
// SettleLine): `goose run --output-format stream-json` closes every session
// with one {"type":"complete",...} line carrying the run's token usage (spike
// §1 — proven live on v1.55.0). It is the last observable "the session's work
// is done" marker on stdout.
const gooseSettleEvent = "complete"

// gooseSettleLine reports whether one goose stream-json stdout line is the
// terminal complete event. Non-JSON lines (the banner preamble) and other
// event types report false: only the run-terminal marker arms the runner's
// quiet window.
func gooseSettleLine(line string) bool {
	var ev struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return false
	}
	return ev.Type == gooseSettleEvent
}

// gooseStreamEvent is one goose stream-json stdout line. The wire is NDJSON:
// one JSON object per line, stderr clean (spike §1). Shapes verified live on
// v1.55.0:
//
//	{"type":"message","message":{"id":"chatcmpl-786","role":"assistant",
//	  "created":1792329755,"content":[{"type":"thinking","thinking":"The",
//	  "signature":""}],"metadata":{...}}}
//	{"type":"message",...,"content":[{"type":"text","text":"SPI"}]}   (deltas)
//	{"type":"message",...,"content":[{"type":"toolRequest","id":"call_…",
//	  "toolCall":{"status":"success","value":{"name":"shell",
//	  "arguments":{"command":"…"}}},"_meta":{"goose_extension":"developer"}}]}
//	{"type":"message",...,"content":[{"type":"toolResponse","id":"call_…",
//	  "toolResult":{"status":"success","value":{"resultType":"complete",
//	  "content":[{"type":"text","text":"…"}],
//	  "structuredContent":{"stdout":"…","stderr":"…","exit_code":0},
//	  "isError":false}}}]}
//	{"type":"complete","total_tokens":10955,"input_tokens":10921,
//	  "output_tokens":34,"cache_read_input_tokens":0,
//	  "cache_write_input_tokens":0}
//
// The message content blocks are chat-completion-shaped; the block
// vocabulary (thinking / text / toolRequest / toolResponse) was enumerated
// from a forced tool loop on the pinned binary (developer builtin, shell
// tool) — the spike had captured only thinking/text, and the Anthropic-style
// tool_use/tool_result blocks the spike draft assumed do NOT exist on this
// wire.
type gooseStreamEvent struct {
	Type    string        `json:"type"`
	Message *gooseMessage `json:"message"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
	// Token usage on the terminal complete event (live captures).
	// Wire numbers fit comfortably in int; UsagePayload consumes int.
	TotalTokens      int `json:"total_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens"`
	CacheWriteTokens int `json:"cache_write_input_tokens"`
}

// gooseMessage is the chat-completion-shaped message chunk: id/role/created +
// content blocks + goose metadata (inference provider/model, visibility,
// operations.llm.advertised_tools — the honest ToolCalls evidence).
type gooseMessage struct {
	ID       string              `json:"id"`
	Role     string              `json:"role"`
	Content  []gooseContentBlock `json:"content"`
	Metadata *struct {
		Inference *struct {
			Provider       string `json:"provider"`
			RequestedModel string `json:"requestedModel"`
		} `json:"inference"`
	} `json:"metadata"`
}

// gooseContentBlock is one message content block — the vocabulary enumerated
// live on v1.55.0 (forced tool loop, see gooseStreamEvent):
//
//	{"type":"thinking","thinking":"…","signature":"…"}     reasoning delta
//	{"type":"text","text":"…"}                             answer delta
//	{"type":"toolRequest","id":"call_…",
//	 "toolCall":{"status":"success","value":{"name":"shell",
//	 "arguments":{…}}},"_meta":{"goose_extension":"developer"}}
//	{"type":"toolResponse","id":"call_…",
//	 "toolResult":{"status":"success","value":{
//	 "resultType":"complete","content":[{"type":"text","text":"…"}],
//	 "structuredContent":{"stdout":"…","stderr":"…","exit_code":0},
//	 "isError":false}}}
//
// toolRequest/toolResponse correlate by id; _meta.goose_extension names the
// MCP extension serving the call (→ ToolPayload.Server). Unknown block types
// are skipped, never fatal — a wire-shape drift degrades progress fidelity,
// not the run.
type gooseContentBlock struct {
	Type string `json:"type"`
	// text block delta.
	Text string `json:"text"`
	// thinking block delta.
	Thinking string `json:"thinking"`
	// toolRequest block: id + the wrapped toolCall (status/value).
	ID       string          `json:"id"`
	ToolCall *gooseToolCall  `json:"toolCall"`
	Meta     *gooseBlockMeta `json:"_meta"`
	// toolResponse block: id + the wrapped toolResult (status/value).
	ToolResult *gooseToolResult `json:"toolResult"`
}

// gooseBlockMeta carries goose's per-block annotations; goose_extension is
// the MCP extension serving the tool (captured: "developer").
type gooseBlockMeta struct {
	GooseExtension string `json:"goose_extension"`
}

// gooseToolResult is the toolResponse payload: status wrapping the MCP-shaped
// result value.
type gooseToolResult struct {
	Status string                `json:"status"`
	Value  *gooseToolResultValue `json:"value"`
}

// gooseToolResultValue is the MCP-shaped result: content text blocks,
// structuredContent (the developer shell tool reports stdout/stderr/exit_code),
// and the isError outcome.
type gooseToolResultValue struct {
	ResultType        string                 `json:"resultType"`
	Content           []gooseResultContent   `json:"content"`
	StructuredContent *gooseStructuredResult `json:"structuredContent"`
	IsError           bool                   `json:"isError"`
}

// gooseToolCall is the toolRequest payload: status ("success" on the captured
// wire; non-success shapes unverified) wrapping the tool name + arguments.
type gooseToolCall struct {
	Status string `json:"status"`
	Value  *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"value"`
}

// gooseResultContent is one MCP result content block ({"type":"text",...} on
// the captured wire; other MCP content kinds decode their fields zero and are
// skipped by the text-only join).
type gooseResultContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// gooseStructuredResult is the developer shell tool's structured payload.
type gooseStructuredResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int64  `json:"exit_code"`
}

// newGooseParser returns the per-run stateful stream-json parser. It owns the
// two jobs the spike decision mandates:
//
//  1. Banner skip: `goose run` prints a ~4-line non-JSON banner before the
//     first JSON line — lines before the first parseable JSON object are
//     dropped silently (never surfaced as message events). AFTER the stream
//     opened, a non-JSON line degrades to an opaque message event (the
//     opencode contract: parse failure must not kill a running task).
//  2. Delta coalescing: message chunks arrive per-token; consecutive
//     same-kind deltas accumulate and flush as ONE typed message event when
//     the kind switches, a tool block arrives, or the run completes — so the
//     wire and telemetry see the answer, not token spam.
//
// State is per-run because Command builds a fresh ExecSpec (hence a fresh
// parser) per Run; no cross-run leakage is possible. toolRequest →
// toolResponse correlation (call id → tool name + serving extension) rides
// the same per-run state.
func newGooseParser() func(line string) []Progress {
	sawJSON := false
	var thinkBuf strings.Builder
	var textBuf strings.Builder
	toolCalls := map[string]gooseToolRef{} // toolRequest id → name + extension

	// flush emits any pending coalesced deltas as message events, thinking
	// first (arrival order: reasoning precedes the answer).
	flush := func(out []Progress) []Progress {
		if thinkBuf.Len() > 0 {
			out = append(out, Progress{
				Kind:    a2a.EventMessage,
				Message: &a2a.MessagePayload{Role: "agent", Text: thinkBuf.String(), Trust: "untrusted"},
			})
			thinkBuf.Reset()
		}
		if textBuf.Len() > 0 {
			out = append(out, Progress{
				Kind:    a2a.EventMessage,
				Message: &a2a.MessagePayload{Role: "agent", Text: textBuf.String(), Trust: "untrusted"},
			})
			textBuf.Reset()
		}
		return out
	}

	return func(line string) []Progress {
		var ev gooseStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			if !sawJSON {
				return nil // banner preamble: skip-until-first-JSON (spike §1)
			}
			return []Progress{{
				Kind:    a2a.EventMessage,
				Message: &a2a.MessagePayload{Role: "agent", Text: line, Trust: "untrusted"},
			}}
		}
		sawJSON = true
		var out []Progress
		switch ev.Type {
		case "message":
			if ev.Message == nil {
				return nil
			}
			for _, blk := range ev.Message.Content {
				switch blk.Type {
				case "thinking":
					// A kind switch flushes the other buffer (order preserved).
					if textBuf.Len() > 0 {
						out = flush(out)
					}
					thinkBuf.WriteString(blk.Thinking)
				case "text":
					if thinkBuf.Len() > 0 {
						out = flush(out)
					}
					textBuf.WriteString(blk.Text)
				case "toolRequest":
					// A tool request flushes pending deltas first (the answer
					// text before a tool call is complete at that point).
					out = flush(out)
					if blk.ToolCall == nil || blk.ToolCall.Value == nil {
						continue // unresolved call shape: bookkeeping, skip
					}
					var server string
					if blk.Meta != nil {
						server = blk.Meta.GooseExtension
					}
					name := blk.ToolCall.Value.Name
					if blk.ID != "" && name != "" {
						toolCalls[blk.ID] = gooseToolRef{name: name, server: server}
					}
					out = append(out, Progress{
						Kind: a2a.EventTool,
						Tool: &a2a.ToolPayload{Name: name, Phase: "start", Server: server},
						// Raw arguments ride the internal seam; the engine's
						// funnel hashes them before they reach any surface.
						ToolArgs: string(blk.ToolCall.Value.Arguments),
					})
				case "toolResponse":
					out = flush(out)
					ref := toolCalls[blk.ID]
					tool := &a2a.ToolPayload{Name: ref.name, Phase: "result", Server: ref.server}
					res := blk.ToolResult
					if res != nil && res.Value != nil {
						ok := !res.Value.IsError
						tool.OK = &ok
						if res.Value.IsError {
							tool.Summary = a2a.ToolErrorSummary(gooseResultText(res.Value))
						}
					} else if res != nil && res.Status == "error" {
						ok := false
						tool.OK = &ok
					}
					out = append(out, Progress{Kind: a2a.EventTool, Tool: tool})
				default:
					// Unknown block type: skip, never fatal.
				}
			}
			return out
		case "complete":
			// Terminal event: flush the last coalesced deltas, then surface
			// the run's token usage as EventUsage (ISI-4238 raw material),
			// cache fields included (present on the v1.55.0 wire). Model and
			// provider stay empty — the engine backfills the launch model
			// before the event leaves the process (pkg/shim/engine.go, same
			// contract as opencode's step_finish). No reasoning tokens are
			// fabricated: on the captured wire total = input + output (+cache),
			// so there is nothing left to attribute.
			out = flush(out)
			return append(out, Progress{
				Kind: a2a.EventUsage,
				Usage: &a2a.UsagePayload{
					Input:      ev.InputTokens,
					Output:     ev.OutputTokens,
					CacheRead:  ev.CacheReadTokens,
					CacheWrite: ev.CacheWriteTokens,
				},
			})
		case "error":
			// Defensive shape: no probe captured a failing run, so the error
			// frame vocabulary on v1.55.0 is unverified. Mapped like
			// opencode's (message + usage-error, plus the shared rate-limit
			// signature matcher) so a provider throttle mid-run still reaches
			// the core's 5.11/2.11 paths IF the frame materializes this way;
			// flip Capabilities.RateLimitSignal only after a live capture.
			name := "goose error"
			detail := name
			if ev.Error != nil && ev.Error.Message != "" {
				detail = ev.Error.Message
			}
			out = append(out,
				Progress{
					Kind:    a2a.EventMessage,
					Message: &a2a.MessagePayload{Role: "agent", Text: detail, Trust: "untrusted"},
				},
				Progress{
					Kind:  a2a.EventUsage,
					Usage: &a2a.UsagePayload{ErrorType: name, Error: detail},
				},
			)
			if isRateLimitError(name, detail) {
				out = append(out, Progress{
					Kind:        a2a.EventRateLimited,
					RateLimited: &a2a.RateLimitedPayload{},
				})
			}
			return out
		default:
			// Unknown event types: bookkeeping, not wire events.
			return nil
		}
	}
}

// gooseToolRef remembers a toolRequest's name and serving extension for the
// correlated toolResponse.
type gooseToolRef struct {
	name   string
	server string
}

// gooseResultText renders an MCP-shaped result value as summary text:
// joined content text blocks, falling back to the structured stdout payload.
func gooseResultText(v *gooseToolResultValue) string {
	parts := make([]string, 0, len(v.Content))
	for _, c := range v.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	if v.StructuredContent != nil {
		return v.StructuredContent.Stdout
	}
	return ""
}
