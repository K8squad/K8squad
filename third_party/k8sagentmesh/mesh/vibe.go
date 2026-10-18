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

// vibeActiveModelEnv pins the model the headless `vibe` runtime serves. In
// non-interactive (`-p`) mode the upstream CLI resolves its active model
// through a GrowthBook feature flag that misfires without a TTY
// (mistralai/mistral-vibe#1140), silently falling back off the role's model.
// Stamping the resolved model here means the standing process serves the right
// model from the first task — the mesh-side half of the mitigation; the
// k8squad shim engine re-pins it per task on the one-shot `vibe` Command.
const vibeActiveModelEnv = "VIBE_ACTIVE_MODEL"

// NewVibeAdapter builds adapter #3 (chapter §9.3 P4.1, ISI-6127): Mistral's
// `vibe` CLI behind the same 7-op RuntimeAdapter seam. Like adapters #1/#2 it
// lands as a NEW ADAPTER ONLY — the gateway, router, lifecycle and supervisor
// keep speaking RuntimeAdapter and never see "vibe". That zero-control-plane
// property is the standing acceptance test the opencode/codex seam established;
// a third runtime exercising it with no control-plane diff is the P4.1 proof.
//
// vibe runs as a standing child process (`shim supervisor` with the runtime
// flavor stamped into its env) on the shared ChildProcessAdapter base, so
// Spawn/Route/Stop/Recreate map onto the shim and Hibernate/Wake are the
// inherited SIGSTOP/SIGCONT base. The per-task CLI shape is one-shot
// (`vibe -p --output streaming ...`, the codex-exec shape) built by the
// k8squad shim engine (pkg/shim/runtimes) behind the supervisor — the module
// never sees it. The runtime-specific halves the module DOES build — the launch
// env and the agent card — come from the injected RuntimeSpec the host
// supplies, so the capability contract the mesh announces is exactly the one
// the host's declarative vibe runtime serves; one source of truth, not two.
//
// host is the substrate the process runs on (LocalHost for the prototype, a
// kube sandbox-pod host later — whose image must carry the uv/Python toolchain
// `vibe` depends on, per the host registry entry).
func NewVibeAdapter(host PlatformHost, spec RuntimeSpec, opts ...Option) (RuntimeAdapter, error) {
	if host == nil {
		return nil, fmt.Errorf("mesh: NewVibeAdapter requires a PlatformHost")
	}
	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("mesh: vibe runtime spec: %w", err)
	}
	return newChildProcessAdapter(
		host,
		spec.Type,
		vibeCard(spec),
		vibeLaunch(spec),
		opts...,
	), nil
}

// vibeCard returns the agent-card builder for vibe. It mirrors the shim
// engine's own card construction, sourcing the honest capability set, default
// model and credential shape from the injected runtime spec so the mesh never
// advertises what the process cannot serve. The shape is identical to adapters
// #1/#2 by design — the card is a pure function of the runtime spec plus the
// envelope, and vibe differs only in the values that spec supplies (CLI pin,
// default model, capabilities, the MISTRAL_API_KEY credential shape).
func vibeCard(spec RuntimeSpec) func(Envelope) a2a.AgentCard {
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

// vibeLaunch returns the LaunchSpec builder for vibe. The standing process is
// `shim supervisor` with the flavor stamped into its env via the injected env
// contract; the supervisor lazily builds its vibe engine on the first routed
// task. Two runtime-specific halves ride the standing-process env from the
// start:
//
//   - VIBE_ACTIVE_MODEL pins the resolved model (the envelope's override, else
//     the runtime default) so the headless GrowthBook bug cannot silently drop
//     the role off its model before the first task arrives.
//   - A BYO model route rides vibe's native credential shape — MISTRAL_BASE_URL
//     for the endpoint and MISTRAL_API_KEY for the token (the Ollama-lane
//     placeholder when the route carries no credential) — exactly the shape the
//     shim engine's per-task Command consumes.
//
// Caller-supplied env (a resolved MISTRAL_API_KEY credential, operator OTLP, …)
// wins over the defaults so a deployment can override the model route or inject
// the real credential without the adapter interpreting it. The env is built
// deterministically (sorted) so a given envelope yields a byte-identical spec
// — the property Recreate relies on for its bare-exec rebuild. The per-process
// source layout (ADR-0024 §1) overrides the plain WorkDir the same way it does
// for adapters #1/#2.
func vibeLaunch(spec RuntimeSpec) func(Envelope) (LaunchSpec, error) {
	return func(env Envelope) (LaunchSpec, error) {
		procEnv := map[string]string{
			spec.RuntimeTypeEnv: spec.Type,
		}
		// Pin the model the headless process serves. The envelope override wins;
		// otherwise the runtime default the host advertised on the card.
		model := spec.DefaultModel.ID
		if env.Model != "" {
			model = env.Model
		}
		if model != "" {
			procEnv[vibeActiveModelEnv] = model
		}
		if env.ModelRoute.Endpoint != "" {
			procEnv["MISTRAL_BASE_URL"] = env.ModelRoute.Endpoint
			key := env.ModelRoute.Token
			if key == "" {
				key = "ollama" // the zero-credential Ollama lane placeholder
			}
			procEnv["MISTRAL_API_KEY"] = key
		}
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
