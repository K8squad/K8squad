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
	"strconv"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
)

// Single-sourced vibe constants (ADR-017 reproducibility; ISI-6158).
const (
	// vibeCLIVersion is the pinned Python vibe release baked into the sandbox
	// image (Dockerfile.toolchain-vibe). Bumped here + image in lockstep.
	vibeCLIVersion = "py-v0.3.0"
	// vibeDefaultModel is the vibe runtime's default model id.
	vibeDefaultModel = "mistral-large-latest"
	// vibeContextWindow is vibeDefaultModel's context-window budget authority
	// for the context Assembler (spec §6.2).
	vibeContextWindow = 131072
	// vibeMaxTurns is the default per-task turn ceiling passed via --max-turns.
	// This prevents a runaway session from holding the sandbox indefinitely;
	// it is a conservative safe ceiling, not a completion hint.
	vibeMaxTurns = 50
	// vibeActiveModelEnv pins the model the headless `vibe` runtime serves,
	// mitigating the upstream headless GrowthBook model-resolution bug
	// (mistralai/mistral-vibe#1140): in non-interactive (-p) mode the CLI
	// resolves its active model through a GrowthBook feature flag that misfires
	// without a TTY, silently falling back off the role's model. Stamping the
	// resolved model here means the one-shot task Command serves the right model.
	// The standing process also receives this env from the mesh adapter (the
	// mesh-side half of the mitigation); re-pinning per task ensures an override
	// route (Agent.spec.model / BYO endpoint) holds for the actual run.
	vibeActiveModelEnv = "VIBE_ACTIVE_MODEL"
	// vibeSettleMarker is the terminal stdout marker the vibe streaming output
	// emits at the end of a completed session. The runner's SettleLine detector
	// uses this to force-settle a completed task whose process lingers
	// (e.g. async cleanup handles or the uv runtime's shutdown latency).
	vibeSettleMarker = "session_end"
)

// vibe is the Mistral vibe coding-agent shim adapter (ISI-6158, P4.1).
// vibe is Mistral's Python coding agent; it speaks the Mistral wire natively,
// so the per-user credential maps onto MISTRAL_API_KEY. The per-task Command
// invokes `vibe -p --output streaming …` (the codex-exec one-shot shape: one
// process per routed task, streaming stdout decoded into typed Progress).
// The uv/Python toolchain vibe requires is provided by the sandbox image
// (Dockerfile.toolchain-vibe); the shim supervisor stamps
// KSQUAD_RUNTIME_TYPE=vibe into the standing-process env and the per-task
// Command re-pins VIBE_ACTIVE_MODEL to mitigate the headless GrowthBook bug
// (mistralai/mistral-vibe#1140).
type vibe struct{}

func (vibe) Type() string                     { return apiv1alpha1.RuntimeTypeVibe }
func (vibe) CLIVersion() string               { return vibeCLIVersion }
func (vibe) CredentialShape() CredentialShape { return ShapeAPIKey }

func (vibe) Capabilities() a2a.Capabilities {
	return a2a.Capabilities{
		Streaming:         true,
		ToolCalls:         true,
		InteractivePrompt: false, // -p (non-interactive) only; no input-required turn
		BYOModelEndpoint:  true,  // MISTRAL_BASE_URL / MISTRAL_API_KEY Ollama lane
		ArtifactKinds:     []string{"file", "patch"},
		Docker:            true,
		GitHub:            true,
		PackageInstall:    true, // uv/Python toolchain in sandbox image
	}
}

func (vibe) DefaultModel() a2a.ModelInfo {
	return a2a.ModelInfo{ID: vibeDefaultModel, ContextWindow: vibeContextWindow}
}

func (r vibe) Command(lc LaunchContext) (ExecSpec, error) {
	// ISI-5329: the large system context spills to a workdir file; only its path
	// rides the env, keeping a token-budgeted context below the kernel's
	// per-env-string ceiling (MAX_ARG_STRLEN, 128 KiB) and preventing exec
	// from dying with E2BIG (ISI-5328).
	env, ctxFile := envelopeFileEnv(lc)

	model := resolveModel(r, lc)
	// Re-pin the active model per task to override any GrowthBook fallback on
	// the headless (non-TTY) standing process (mistralai/mistral-vibe#1140).
	// Caller-supplied env wins, so this stamp is the safe floor.
	env = append(env, vibeActiveModelEnv+"="+model)

	// Mistral credential + BYO endpoint. Vibe speaks the Mistral wire natively,
	// so a BYO route uses MISTRAL_BASE_URL/MISTRAL_API_KEY (Ollama lane
	// placeholder when no token is set). Caller env wins over these defaults.
	if lc.ModelRoute.Endpoint != "" {
		env = append(env, "MISTRAL_BASE_URL="+lc.ModelRoute.Endpoint)
		key := lc.ModelRoute.Token
		if key == "" {
			key = "ollama" // unauthenticated Ollama lane placeholder
		}
		env = append(env, "MISTRAL_API_KEY="+key)
	} else if lc.Credential != "" {
		env = append(env, "MISTRAL_API_KEY="+lc.Credential)
	}

	return ExecSpec{
		Path: "vibe",
		Args: []string{
			"-p",
			"--output", "streaming",
			"--auto-approve",
			"--workdir", lc.WorkDir,
			"--max-turns", strconv.Itoa(vibeMaxTurns),
			"--trust",
			"--model", model,
		},
		Env:          env,
		WorkDir:      lc.WorkDir,
		WorkDirFiles: []WorkDirFile{ctxFile},
		Parse:        parseVibeLine,
		// ISI-4224: vibe emits a terminal session_end JSON marker when its
		// session completes. The runner's SettleLine quiet window force-settles
		// the task after this marker, so lingering async handles (uv cleanup,
		// remote fetch retries) do not hold the process open and block
		// settlement indefinitely.
		SettleLine: vibeSettleLine,
	}, nil
}

func init() { Register(vibe{}) }

// vibeSettleLine reports whether one vibe --output streaming stdout line is
// the terminal session_end event. Non-JSON lines and other event types return
// false; only the exact terminal marker triggers the settle window.
func vibeSettleLine(line string) bool {
	var ev struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return false
	}
	return ev.Type == vibeSettleMarker
}

// vibeEvent is the discriminated shape of one vibe --output streaming NDJSON
// line. The discriminant is Type; each kind populates its own fields.
// Token counters on the "usage" event type ride at the top level (flat, not
// nested), so they are declared directly on the envelope struct.
type vibeEvent struct {
	Type string `json:"type"`
	// tool_call: the tool name + arguments. Status is the terminal state.
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Status    string          `json:"status"`
	ExitCode  *int            `json:"exit_code"`
	// agent_message: the text the agent emitted.
	Text string `json:"text"`
	// usage event: top-level token counters (flat wire shape).
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// error text on session_failed / error events.
	Error   string `json:"error"`
	Message string `json:"message"`
}

// parseVibeLine decodes one vibe --output streaming NDJSON stdout line into
// typed Progress events. The vibe streaming wire shapes (py-v0.3.0):
//
//	{"type":"tool_call","tool":"bash","arguments":{"cmd":"git status"},"status":"completed","exit_code":0}
//	{"type":"agent_message","text":"Done — 3 files changed."}
//	{"type":"usage","input_tokens":1240,"output_tokens":87}
//	{"type":"session_end"}
//	{"type":"session_failed","error":"max turns reached"}
//
// A non-JSON line (diagnostics / tracebacks) degrades to an untrusted message
// event — parse failure MUST NOT kill a running task. session_end itself is
// not emitted as Progress; the runner's SettleLine detector handles it.
func parseVibeLine(line string) []Progress {
	var ev vibeEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: line, Trust: "untrusted"},
		}}
	}
	switch ev.Type {
	case "tool_call":
		if ev.Tool == "" {
			return nil
		}
		tool := &a2a.ToolPayload{Name: ev.Tool, Phase: "result"}
		if ev.ExitCode != nil {
			ok := *ev.ExitCode == 0
			tool.OK = &ok
		} else {
			tool.OK = vibeStatusOK(ev.Status)
		}
		var args string
		if len(ev.Arguments) > 0 {
			args = string(ev.Arguments)
		}
		return []Progress{{Kind: a2a.EventTool, Tool: tool, ToolArgs: args}}
	case "agent_message":
		if ev.Text == "" {
			return nil
		}
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: ev.Text, Trust: "untrusted"},
		}}
	case "usage":
		if u := vibeUsagePayload(ev.InputTokens, ev.OutputTokens); u != nil {
			return []Progress{{Kind: a2a.EventUsage, Usage: u}}
		}
		return nil
	case "session_end":
		// Handled by SettleLine; no Progress emitted.
		return nil
	case "session_failed", "error":
		msg := ev.Error
		if msg == "" {
			msg = ev.Message
		}
		if msg == "" {
			msg = "vibe session failed"
		}
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: msg, Trust: "untrusted"},
		}}
	default:
		return nil
	}
}

// vibeStatusOK maps a vibe tool_call terminal status onto the tri-state tool
// outcome: "completed" = ok, "failed"/"error" = not ok, otherwise unknown.
func vibeStatusOK(status string) *bool {
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

// vibeUsagePayload maps vibe usage counters onto an EventUsage payload.
// Returns nil when both counters are zero (no meaningful usage to emit).
func vibeUsagePayload(inputTokens, outputTokens int) *a2a.UsagePayload {
	if inputTokens == 0 && outputTokens == 0 {
		return nil
	}
	return &a2a.UsagePayload{
		Input:  inputTokens,
		Output: outputTokens,
	}
}
