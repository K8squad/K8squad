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

// hermes is the Hermes shim adapter (story 5.5). Hermes runs autonomously with
// no interactive-prompt turn, so it advertises interactivePrompt=false as a
// first-class capability gap (story 5.2, F15) rather than dead-ending a Run
// that reaches an input-required state. It is otherwise a full coding agent
// on the OpenAI-compatible wire.
type hermes struct{}

func (hermes) Type() string                     { return apiv1alpha1.RuntimeTypeHermes }
func (hermes) CLIVersion() string               { return "v1.2.0" }
func (hermes) CredentialShape() CredentialShape { return ShapeAPIKey }

func (hermes) Capabilities() a2a.Capabilities {
	return a2a.Capabilities{
		Streaming:         true,
		ToolCalls:         true,
		InteractivePrompt: false, // honest gap: Hermes has no input-required turn
		BYOModelEndpoint:  true,
		ArtifactKinds:     []string{"file"},
		Docker:            true,
		GitHub:            true,
		PackageInstall:    true,
	}
}

func (hermes) DefaultModel() a2a.ModelInfo {
	return a2a.ModelInfo{ID: "gpt-4o", ContextWindow: 128000}
}

func (r hermes) Command(lc LaunchContext) (ExecSpec, error) {
	env := envelopeEnv(lc)
	if lc.Credential != "" {
		env = append(env, "HERMES_API_KEY="+lc.Credential)
	}
	env = append(env, modelRouteEnv(lc.ModelRoute)...)
	// Epic C (ADR-044 step 6): hermes consumes the normalized IR
	// directly — passthrough, no native render.
	if len(lc.MCPEndpoints) > 0 {
		if raw, err := capability.RenderHermes(lc.MCPEndpoints); err != nil {
			return ExecSpec{}, err
		} else {
			env = append(env, "HERMES_MCP_CONFIG="+string(raw))
		}
	}
	return ExecSpec{
		Path:    "hermes",
		Args:    []string{"agent", "--output=json", "--model", resolveModel(r, lc)},
		Env:     env,
		WorkDir: lc.WorkDir,
	}, nil
}

func init() { Register(hermes{}) }
