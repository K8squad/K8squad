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

// openClaw is the OpenClaw shim adapter (story 5.5). OpenClaw is a full coding
// agent: it streams progress, calls tools, drives Docker/GitHub, installs
// packages, and speaks the OpenAI-compatible wire, so it honors the BYO model
// endpoint (story 5.7) and can prove conformance on the $0 Ollama lane.
type openClaw struct{}

func (openClaw) Type() string { return apiv1alpha1.RuntimeTypeOpenClaw }

// CLIVersion names the PLANNED OpenClaw CLI revision: there is no upstream
// release artifact to pin yet, so ksquad-shim-openclaw ships no CLI
// (intentionally unpackaged — see the cli-openclaw stage disposition in
// Dockerfile.shim, ISI-3667).
func (openClaw) CLIVersion() string               { return "v0.9.0" }
func (openClaw) CredentialShape() CredentialShape { return ShapeAPIKey }

func (openClaw) Capabilities() a2a.Capabilities {
	return a2a.Capabilities{
		Streaming:         true,
		ToolCalls:         true,
		InteractivePrompt: true,
		BYOModelEndpoint:  true,
		ArtifactKinds:     []string{"file", "patch"},
		Docker:            true,
		GitHub:            true,
		PackageInstall:    true,
	}
}

func (openClaw) DefaultModel() a2a.ModelInfo {
	return a2a.ModelInfo{ID: "claude-sonnet-4", ContextWindow: 200000}
}

func (r openClaw) Command(lc LaunchContext) (ExecSpec, error) {
	env := envelopeEnv(lc)
	// Credential → native OpenClaw key (story 5.4). Never logged.
	if lc.Credential != "" {
		env = append(env, "OPENCLAW_API_KEY="+lc.Credential)
	}
	env = append(env, modelRouteEnv(lc.ModelRoute)...)
	spec := ExecSpec{
		Path:    "openclaw",
		Args:    []string{"run", "--format=json", "--model=" + resolveModel(r, lc)},
		Env:     env,
		WorkDir: lc.WorkDir,
	}
	// Epic C (ADR-044 step 6): openclaw.json's mcp.servers section,
	// rendered from the projected IR at start.
	if f, err := mcpWorkDirFile("openclaw.json", capability.RenderOpenClaw, lc.MCPEndpoints); err != nil {
		return ExecSpec{}, err
	} else if f != nil {
		spec.WorkDirFiles = append(spec.WorkDirFiles, *f)
	}
	return spec, nil
}

func init() { Register(openClaw{}) }
