/*
Copyright 2026 The K8sAgentMesh Authors.

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

package mesh

import (
	"fmt"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// claudeCodeModelEnv pins the standing process's default model
// (ANTHROPIC_MODEL, the CLI's documented model-select env — the goose
// GOOSE_MODEL pin analogue). It mirrors the engine-side constant
// (agentruntime/claudecode.go) by design: the standing process stamps it
// once and the per-task one-shot re-pins via --model.
const claudeCodeModelEnv = "ANTHROPIC_MODEL"

// NewClaudeCodeAdapter builds adapter #5 (chapter §9.3 P4.4, ISI-6130):
// Anthropic's Claude Code behind the same 7-op RuntimeAdapter seam. Like
// adapters #1–#4 it lands as a NEW ADAPTER ONLY — the gateway, router,
// lifecycle and supervisor keep speaking RuntimeAdapter and never see
// "claude-code"; a fifth runtime exercising that surface with no
// control-plane diff is the P4.4 mesh-side proof.
//
// The standing process is `shim supervisor` with the claude-code flavor
// stamped into its env on the shared ChildProcessAdapter base; the supervisor
// lazily builds its engine (agentruntime, registered for claude-code by the
// same ISI-6130 pass) on the first routed task, and the per-task surface is
// the one-shot `claude -p --output-format stream-json --verbose` invocation.
// The per-user credential — the CLAUDE_CODE_OAUTH_TOKEN subscription seat
// (story 7.2) — rides the envelope's caller-supplied env, so the adapter
// never touches or interprets secret material.
//
// host is the substrate the process runs on (LocalHost for the prototype, a
// kube sandbox-pod host later — whose image carries the pinned claude CLI).
func NewClaudeCodeAdapter(host PlatformHost, spec RuntimeSpec, opts ...Option) (RuntimeAdapter, error) {
	if host == nil {
		return nil, fmt.Errorf("mesh: NewClaudeCodeAdapter requires a PlatformHost")
	}
	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("mesh: claude-code runtime spec: %w", err)
	}
	return newChildProcessAdapter(
		host,
		spec.Type,
		claudeCodeCard(spec),
		claudeCodeLaunch(spec),
		opts...,
	), nil
}

// claudeCodeCard returns the agent-card builder for claude-code. It mirrors
// the shim engine's own card construction, sourcing the honest capability
// set, default model and credential shape from the injected runtime spec so
// the mesh never advertises what the process cannot serve. The shape is
// identical to adapters #1–#4 by design — the card is a pure function of the
// runtime spec plus the envelope, and claude-code differs only in the values
// that spec supplies: the CLI pin, the Claude-family default model, the
// oauth-token credential shape, and a capability set with the BYO
// OpenAI-compatible endpoint advertised as a gap (the CLI speaks the
// Anthropic wire).
func claudeCodeCard(spec RuntimeSpec) func(Envelope) a2a.AgentCard {
	return func(env Envelope) a2a.AgentCard {
		model := spec.DefaultModel
		if env.Model != "" {
			model.ID = env.Model // context window stays the runtime's authority
		}
		skills := env.Skills
		if skills == nil {
			skills = []string{}
		}
		return a2a.AgentCard{
			SchemaVersion: spec.schemaVersion(),
			Agent: a2a.AgentIdentity{
				Name:    env.Identity.Name,
				Squad:   env.Identity.Squad,
				Project: env.Identity.Project,
			},
			Runtime: a2a.RuntimeInfo{
				Type:        spec.Type,
				CLIVersion:  spec.CLIVersion,
				ShimVersion: shimVersion,
			},
			Model:  model,
			Skills: skills,
			Auth: a2a.AuthInfo{
				Type: spec.CredentialType,
			},
			Capabilities: spec.Capabilities,
			Protocol:     spec.Protocol,
		}
	}
}

// claudeCodeLaunch returns the LaunchSpec builder for claude-code. The
// standing process is `shim supervisor` with the flavor stamped into its env
// via the injected env contract; the supervisor lazily builds its claude-code
// engine on the first routed task. Beyond the shared shape, the standing
// process carries the ANTHROPIC_MODEL pin — the envelope's override, else the
// runtime default — so the process serves the role's model from the first
// task; the engine re-pins per task via --model so an override holds. A BYO
// OpenAI-compatible ModelRoute is deliberately NOT stamped: claude-code
// speaks the Anthropic wire, so the OPENAI_* lane other adapters stamp would
// be ignored (the card advertises the gap honestly). The per-user OAuth
// credential arrives as caller-supplied env (env.Env) and wins over every
// default. The env is built deterministically (sorted) so a given envelope
// yields a byte-identical spec — the property Recreate relies on for its
// bare-exec rebuild. The per-process source layout (ADR-0024 §1) overrides
// the plain WorkDir the same way it does for adapters #1–#4.
func claudeCodeLaunch(spec RuntimeSpec) func(Envelope) (LaunchSpec, error) {
	return func(env Envelope) (LaunchSpec, error) {
		procEnv := map[string]string{
			spec.RuntimeTypeEnv: spec.Type,
		}
		// Pin the model the headless process serves. The envelope override
		// wins; otherwise the runtime default the host advertised on the card.
		model := spec.DefaultModel.ID
		if env.Model != "" {
			model = env.Model
		}
		if model != "" {
			procEnv[claudeCodeModelEnv] = model
		}
		// Caller-supplied env (the resolved CLAUDE_CODE_OAUTH_TOKEN seat,
		// operator OTLP, …) wins over the defaults so a deployment injects a
		// credential without the adapter interpreting it.
		for k, v := range env.Env {
			procEnv[k] = v
		}
		launch := LaunchSpec{
			RuntimeType: spec.Type,
			Env:         sortedEnv(procEnv),
			WorkDir:     env.WorkDir,
			Identity:    env.Identity,
			Source:      env.Source,
		}
		if env.Source != nil {
			launch.WorkDir = env.Source.WorktreeDir
		}
		return launch, nil
	}
}
