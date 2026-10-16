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
	"sort"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// shimVersion is the mesh-side shim revision stamped on the agent card. The
// standing process serves its own card over A2A at runtime; this is the
// envelope-time card the gateway registers before the first route, so it mirrors
// the shim's own default rather than claiming a pinned build it cannot see.
const shimVersion = "mesh-p1"

// RuntimeSpec is the injected runtime profile a host supplies to a concrete
// child-process adapter (leak L1, ISI-5827): everything the card and launch
// builders need from the host's declarative runtime registry, as plain values.
// The module never imports a host registry — the k8squad host plugin supplies
// this spec from its own sources of truth (pkg/shim/runtimes, internal/
// protocol, its SchemaVersion stamp and its runtime-type constants), so there
// stays one source of truth per host, never two.
type RuntimeSpec struct {
	// Type is the runtime flavor name (k8squad host: api/v1alpha1 RuntimeType
	// constants). Stamped on the agent card, the LaunchSpec and the launch env
	// so the standing process serves — and advertises — the right flavor.
	Type string
	// CLIVersion is the pinned coding-agent CLI revision the adapter targets
	// (ADR-017 reproducibility). Advertised on the agent card.
	CLIVersion string
	// Capabilities is the honest, first-class capability set the process
	// serves: a gap is advertised, never discovered as a mid-Run failure.
	// Advertised on the agent card.
	Capabilities a2a.Capabilities
	// DefaultModel is the runtime's default model + its context-window
	// authority. The envelope's model override replaces the id, never the
	// window.
	DefaultModel a2a.ModelInfo
	// CredentialType is the native credential shape the process consumes
	// (a2a.AuthInfo.Type on the card; k8squad host: the CredentialShape its
	// declarative runtime maps the generic Secret to).
	CredentialType string
	// SchemaVersion is the host-stamped Agent Card schema tag (spec §6.1).
	// Empty uses the module default (a2a.DefaultSchemaVersion); the k8squad
	// host stamps "ksquad.a2a/v1".
	SchemaVersion string
	// Protocol is the host's pinned wire revisions the card advertises
	// (spec §6.1; k8squad host: internal/protocol.Pinned()).
	Protocol a2a.ProtocolVersions
	// RuntimeTypeEnv is the env contract the standing process's supervisor
	// reads to pick its baked-in runtime flavor (k8squad host:
	// KSQUAD_RUNTIME_TYPE, read by cmd/shim/supervisor.go runtime()).
	// Spawn stamps it so the standing process serves the right flavor.
	RuntimeTypeEnv string
}

// validate rejects a spec the launch/card builders cannot run on. It is the
// injected-seam analogue of the host registry's "runtime not registered"
// failure: the constructor fails loudly at wiring time, never mid-Spawn.
func (s RuntimeSpec) validate() error {
	if s.Type == "" {
		return fmt.Errorf("runtime Type is required")
	}
	if s.RuntimeTypeEnv == "" {
		return fmt.Errorf("runtime RuntimeTypeEnv (launch env contract) is required")
	}
	return nil
}

// schemaVersion resolves the card's schema tag: the host's stamp when set, the
// module default otherwise.
func (s RuntimeSpec) schemaVersion() string {
	if s.SchemaVersion != "" {
		return s.SchemaVersion
	}
	return a2a.DefaultSchemaVersion
}

// NewOpenCodeAdapter builds adapter #1 (chapter §1B, D8.1): opencode wrapped
// behind the 7-op RuntimeAdapter. It is a ChildProcessAdapter — opencode runs
// as a standing child process (`shim supervisor` with the runtime flavor
// stamped into its env), so Spawn/Route/Stop/Recreate map onto the existing
// shim and Hibernate/Wake are the inherited SIGSTOP/SIGCONT base. The
// runtime-specific halves — the launch env and the agent card — are built
// from the injected RuntimeSpec the host supplies, so the capability contract
// the mesh announces is exactly the one the host's declarative runtime serves;
// there is one source of truth, not two.
//
// host is the substrate the process runs on (LocalHost for the prototype, a
// kube sandbox-pod host later). The returned adapter speaks only RuntimeAdapter;
// the gateway/supervisor never see "opencode" — which is the test that adding a
// second runtime at P2 is a new adapter with zero control-plane change.
func NewOpenCodeAdapter(host PlatformHost, spec RuntimeSpec, opts ...Option) (RuntimeAdapter, error) {
	if host == nil {
		return nil, fmt.Errorf("mesh: NewOpenCodeAdapter requires a PlatformHost")
	}
	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("mesh: opencode runtime spec: %w", err)
	}
	return newChildProcessAdapter(
		host,
		spec.Type,
		openCodeCard(spec),
		openCodeLaunch(spec),
		opts...,
	), nil
}

// openCodeCard returns the agent-card builder for opencode: it mirrors the
// shim engine's own card construction, sourcing the honest capability set,
// default model and credential shape from the injected runtime spec so the
// mesh never drifts from what the process serves.
func openCodeCard(spec RuntimeSpec) func(Envelope) a2a.AgentCard {
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

// openCodeLaunch returns the LaunchSpec builder for opencode. The standing
// process is `shim supervisor` with the flavor stamped into its env via the
// injected env contract; the supervisor lazily builds its opencode engine on
// the first routed task. The envelope's model route and extra env (a resolved
// credential) are layered on so the process serves the right model before any
// task arrives — the "pre-loaded with the role envelope" half of Spawn. The
// env is built deterministically (sorted) so a given envelope yields a
// byte-identical spec, which keeps Recreate a true bare-exec rebuild.
func openCodeLaunch(spec RuntimeSpec) func(Envelope) (LaunchSpec, error) {
	return func(env Envelope) (LaunchSpec, error) {
		procEnv := map[string]string{
			spec.RuntimeTypeEnv: spec.Type,
		}
		// A BYO model route rides the OpenAI-compatible env the opencode runtime
		// honors; the shim renders its own opencode.json provider block per task,
		// but the endpoint/key must be present on the process from the start.
		if env.ModelRoute.Endpoint != "" {
			procEnv["OPENAI_BASE_URL"] = env.ModelRoute.Endpoint
			key := env.ModelRoute.Token
			if key == "" {
				key = "ollama" // the zero-credential Ollama lane placeholder
			}
			procEnv["OPENAI_API_KEY"] = key
		}
		// Caller-supplied env (resolved credential, operator OTLP, …) wins over
		// the defaults so a deployment can override the model route or inject a
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
		// Per-process write discipline (ISI-6009, ADR-0024 §1): the source layout
		// wins over the plain WorkDir — the process runs in its own worktree.
		if env.Source != nil {
			launch.WorkDir = env.Source.WorktreeDir
		}
		return launch, nil
	}
}

// sortedEnv renders a KEY=VALUE environment slice in sorted key order so an
// envelope maps to a deterministic LaunchSpec (stable Recreate, testable Spawn).
func sortedEnv(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
