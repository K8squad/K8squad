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
		Path:    "opencode",
		Args:    []string{"run", "--print-logs", "--format=json", "--model", modelFlag},
		Env:     env,
		WorkDir: lc.WorkDir,
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
