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

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
)

// openCode is the opencode shim adapter (story 5.8). opencode is a
// provider-agnostic terminal coding agent built around the OpenAI-compatible
// wire, so it is a natural fit for the BYO model endpoint (story 5.7) and the
// Ollama conformance lane (story 5.6). It runs package installs through its own
// sandbox rather than the shim, so it advertises packageInstall=false honestly.
type openCode struct{}

func (openCode) Type() string { return apiv1alpha1.RuntimeTypeOpenCode }

// CLIVersion pins the sst/opencode release baked into ksquad-shim-opencode
// (Dockerfile.shim cli-opencode, OPENCODE_VERSION — keep the two in lockstep,
// ADR-017). Bumped v0.4.0 → v1.18.27 by ISI-3667: v0.4.0 predates upstream's
// musl release assets the distroless/static shim image requires.
func (openCode) CLIVersion() string               { return "v1.18.27" }
func (openCode) CredentialShape() CredentialShape { return ShapeAPIKey }

func (openCode) Capabilities() a2a.Capabilities {
	return a2a.Capabilities{
		Streaming:         true,
		ToolCalls:         true,
		InteractivePrompt: true,
		BYOModelEndpoint:  true,
		ArtifactKinds:     []string{"file", "patch"},
		Docker:            true,
		GitHub:            true,
		PackageInstall:    false, // opencode owns its own package sandbox
	}
}

func (openCode) DefaultModel() a2a.ModelInfo {
	return a2a.ModelInfo{ID: "claude-sonnet-4", ContextWindow: 200000}
}

func (r openCode) Command(lc LaunchContext) (ExecSpec, error) {
	env := envelopeEnv(lc)
	if lc.Credential != "" {
		env = append(env, "OPENCODE_API_KEY="+lc.Credential)
	}
	env = append(env, modelRouteEnv(lc.ModelRoute)...)
	model := resolveModel(r, lc)
	// ISI-4224: opencode v1.18.27's non-interactive run can leave handles
	// holding the Bun event loop open after the session went idle — the
	// auto-update check, the project file watcher, and (on BYO runs) the
	// models.dev fetch. Suppress them at the source so the process exits
	// on its own and the runner's EOF fast path stays the norm; the
	// runner's SettleLine quiet window is the backstop for any linger
	// source these flags do not cover. models.dev fetch is disabled ONLY
	// on BYO runs, where the rendered provider block (opencode.json) is
	// self-contained — vendor runs still need models.dev provider metadata.
	env = append(env, "OPENCODE_DISABLE_AUTOUPDATE=1")
	env = append(env, "OPENCODE_EXPERIMENTAL_DISABLE_FILEWATCHER=1")
	if lc.ModelRoute.Endpoint != "" {
		env = append(env, "OPENCODE_DISABLE_MODELS_FETCH=1")
	}
	// ISI-4188 gap 2: with a BYO endpoint the model must address the rendered
	// provider block (opencode.json provider.<ksquad-byo>) — opencode v1.18.27
	// resolves --model as <provider>/<model> and does NOT resolve providers
	// from OPENAI_BASE_URL env (verified live: env-only yields
	// ProviderModelNotFoundError).
	modelFlag := model
	if lc.ModelRoute.Endpoint != "" {
		modelFlag = capability.OpenCodeBYOProviderID + "/" + model
	}
	spec := ExecSpec{
		Path: "opencode",
		Args: []string{"run", "--print-logs", "--format=json", "--model", modelFlag},
		Env:  env,
		// ISI-4188 gap 5: opencode `run` reads its message from argv or stdin
		// and errors "You must provide a message or a command" with neither.
		// The prompt rides stdin (NFR-SEC1: never argv), the same channel the
		// codex wrapper uses.
		Stdin: EnvelopePrompt(lc),
		// ISI-4188 gap 7: --format=json emits one JSON event per line
		// (step_start/tool_use/text/step_finish/error); decode them into
		// typed Progress so tool calls reach the wire + telemetry as
		// EventTool, not opaque text.
		Parse:      parseOpenCodeLine,
		WorkDir:    lc.WorkDir,
		SettleLine: openCodeSettleLine,
	}
	// Epic C (ADR-044 step 6): opencode.json's mcp section, rendered from
	// the projected IR at start (native tools.enable scoping). ISI-4188
	// gap 2: a BYO model route ALSO rides this file (provider block) —
	// rendered whenever either half is set.
	if len(lc.MCPEndpoints) > 0 || lc.ModelRoute.Endpoint != "" {
		content, err := capability.RenderOpenCodeConfig(lc.MCPEndpoints, lc.ModelRoute.Endpoint, model)
		if err != nil {
			return ExecSpec{}, err
		}
		spec.WorkDirFiles = append(spec.WorkDirFiles, WorkDirFile{Name: "opencode.json", Content: content})
	}
	return spec, nil
}

func init() { Register(openCode{}) }

// openCodeSettleEvent is the opencode stdout line type the ISI-4224 settle
// detector matches: upstream cmd/run.ts (v1.18.27) emits one
// {"type":"step_finish",…} JSON line per finished agent step and breaks its
// own event loop on the session-idle that follows the LAST one — the idle
// transition itself is never printed, so the final step_finish is the last
// observable "the session's work is done" marker on stdout.
const openCodeSettleEvent = "step_finish"

// openCodeSettleLine reports whether one opencode --format=json stdout line
// is a step_finish event (ISI-4224). Non-JSON lines and other event types
// (step_start, text, tool_use, …) report false: only the per-step terminal
// marker arms the runner's quiet window, and a subsequent step's first line
// re-arms it — so a multi-step session is never truncated mid-flight.
func openCodeSettleLine(line string) bool {
	var ev struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return false
	}
	return ev.Type == openCodeSettleEvent
}

// parseOpenCodeLine decodes one `--format=json` stdout line into typed
// Progress events (ISI-4188 gap 7). The shapes below are the opencode
// v1.18.27 wire, verified live on k8squad-test against the pinned image:
//
//	{"type":"tool_use","part":{"tool":"read","state":{"status":"completed",
//	  "input":{...},"output":"...","time":{"start":…,"end":…}}}}
//	{"type":"text","part":{"text":"…"}}
//	{"type":"error","error":{"name":"…","data":{"message":"…"}}}
//	{"type":"step_start",…}          (progress bookkeeping; dropped)
//	{"type":"step_finish","part":{"type":"step-finish","providerID":"…",
//	  "modelID":"…","tokens":{"input":…,"output":…,"reasoning":…,
//	  "cache":{"read":…,"write":…}},"cost":{…},"duration":…}}
//
// A tool_use event arrives once per call with a terminal state.status
// (completed|error) — it maps to a single phase="result" ToolPayload; the
// telemetry spine settles one span per call from it. A non-JSON line (the
// CLI's own diagnostics interleave on stdout) degrades to a message event,
// never an error: parse failure must not kill a running task.
//
// step_finish maps to an EventUsage (ISI-4238): the per-step token/cost
// record is the raw material for the llm.call span, the run's token totals
// and the interaction views. A step_finish without tokens (older wire,
// bookkeeping-only) stays dropped. The model id rides providerID/modelID;
// when the part omits them the engine fills the launch model before the
// event leaves the process.
// openCodePart is the union `part` block of the opencode JSON wire: text
// parts, tool_use parts and step-finish parts all ride the same envelope,
// each populating its own fields.
type openCodePart struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	Tool       string `json:"tool"`
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
	State      *struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
	} `json:"state"`
	// Tokens is the step-finish usage block; nil on shapes that do not
	// carry it (then step_finish stays bookkeeping, ISI-4238).
	Tokens *struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     *struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Cost *struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
		Total  float64 `json:"total"`
	} `json:"cost"`
	DurationMS *int64 `json:"duration"`
}

func parseOpenCodeLine(line string) []Progress {
	var ev struct {
		Type  string        `json:"type"`
		Part  *openCodePart `json:"part"`
		Error *struct {
			Name string `json:"name"`
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: line, Trust: "untrusted"},
		}}
	}
	switch ev.Type {
	case "tool_use":
		if ev.Part == nil || ev.Part.State == nil {
			return nil
		}
		tool := &a2a.ToolPayload{Name: ev.Part.Tool}
		var args string
		if len(ev.Part.State.Input) > 0 {
			args = string(ev.Part.State.Input)
		}
		switch ev.Part.State.Status {
		case "completed":
			ok := true
			tool.Phase = "result"
			tool.OK = &ok
		case "error":
			ok := false
			tool.Phase = "result"
			tool.OK = &ok
		default: // pending/running — the call is in flight
			tool.Phase = "start"
		}
		return []Progress{{Kind: a2a.EventTool, Tool: tool, ToolArgs: args}}
	case "text":
		if ev.Part == nil || ev.Part.Text == "" {
			return nil
		}
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: ev.Part.Text, Trust: "untrusted"},
		}}
	case "step_finish":
		if usage := usageFromStepFinish(ev.Part); usage != nil {
			return []Progress{{Kind: a2a.EventUsage, Usage: usage}}
		}
		return nil
	case "error":
		msg := "opencode error"
		if ev.Error != nil {
			msg = ev.Error.Name
			if ev.Error.Data.Message != "" {
				msg = msg + ": " + ev.Error.Data.Message
			}
		}
		return []Progress{{
			Kind:    a2a.EventMessage,
			Message: &a2a.MessagePayload{Role: "agent", Text: msg, Trust: "untrusted"},
		}}
	default: // step_start and future shapes: bookkeeping, not wire events
		return nil
	}
}

// usageFromStepFinish maps a step-finish part onto an EventUsage payload
// (ISI-4238). It returns nil when the part is absent or carries no token
// block — a bare step_finish stays dropped exactly as before, so a
// wire-shape drift degrades to the prior behavior instead of emitting
// zeroed usage that would poison the run's token totals.
func usageFromStepFinish(part *openCodePart) *a2a.UsagePayload {
	if part == nil || part.Tokens == nil {
		return nil
	}
	u := &a2a.UsagePayload{
		Input:     part.Tokens.Input,
		Output:    part.Tokens.Output,
		Reasoning: part.Tokens.Reasoning,
	}
	switch {
	case part.ProviderID != "" && part.ModelID != "":
		u.Model = part.ProviderID + "/" + part.ModelID
	default:
		u.Model = part.ModelID
	}
	if part.Tokens.Cache != nil {
		u.CacheRead = part.Tokens.Cache.Read
		u.CacheWrite = part.Tokens.Cache.Write
	}
	if part.Cost != nil {
		u.CostUSD = part.Cost.Total
	}
	if part.DurationMS != nil {
		u.DurationMS = *part.DurationMS
	}
	return u
}
