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
		Path:       "opencode",
		Args:       []string{"run", "--print-logs", "--format=json", "--model", modelFlag},
		Env:        env,
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
