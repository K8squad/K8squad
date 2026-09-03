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
	}
	// Epic C (ADR-044 step 6): codex's config.toml [mcp_servers.*] section,
	// rendered from the projected IR at start. RenderCodex is the S5 renderer
	// (stubbed until then); credentials ride as env NAMES inside the document.
	if f, err := mcpWorkDirFile("config.toml", capability.RenderCodex, lc.MCPEndpoints); err != nil {
		return ExecSpec{}, err
	} else if f != nil {
		spec.WorkDirFiles = append(spec.WorkDirFiles, *f)
	}
	return spec, nil
}

func init() { Register(codex{}) }
