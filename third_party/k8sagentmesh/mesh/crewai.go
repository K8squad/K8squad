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
	"context"
	"fmt"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// The CrewAI flavor env contract, locked by the ISI-6547 spike against the
// pinned stack (crewai[a2a] 1.15.28 → a2a-sdk ~=0.3.10; see
// docs/crewai-integration.md): every name below was verified against the
// pinned package, not assumed.
const (
	// crewAuthTokenEnv is the A2A server's auth token env var. CrewAI's
	// A2AServerConfig auth defaults to SimpleTokenAuth, which reads AUTH_TOKEN
	// from the process env — the host resolves the secret and layers it onto
	// the envelope env (Envelope.Env), and the router the host wires must
	// carry the same value as its Bearer token.
	crewAuthTokenEnv = "AUTH_TOKEN"
	// crewOpenAIBaseURLEnv / crewOpenAIAPIKeyEnv are the OpenAI-compatible
	// model-route env vars CrewAI's litellm lane consumes: a BYO ModelRoute
	// rides them verbatim (the same shape adapters #1–#4 stamp for their BYO
	// lanes). Crew model ids themselves stay crew-author-side — the crew
	// project owns its agents and their models in code.
	crewOpenAIBaseURLEnv = "OPENAI_BASE_URL"
	crewOpenAIAPIKeyEnv  = "OPENAI_API_KEY"
)

// NewCrewAdapter builds adapter #5 (chapter §9.3 P4.3, ISI-6547 — the mesh
// half of the accepted ISI-6129 design): CrewAI behind the same 7-op
// RuntimeAdapter seam, as a wrapper over the shared ChildProcessAdapter base
// whose standing child process is the crew's own A2A server (python +
// crewai[a2a], pinned by the host's sandbox image) rather than a coding CLI.
//
// Two properties distinguish it from adapters #1–#4, both locked by the
// design's Decision A:
//
//   - The default Router is the crewai wire translator (crewaiwire.go), not
//     the module /task HTTP router — the crew speaks Google A2A 0.3.x
//     JSON-RPC+SSE on its default binding, and crewaiRouter maps it onto the
//     module wire inside this adapter seam (R11: wire churn is absorbed at
//     the adapter, never in the control plane). WithRouter still overrides it
//     for tests and alternate transports.
//   - Hibernate/Wake are cooperative telemetry-emitting no-ops. The crew is
//     an asyncio server that may hold open SSE connections; a SIGSTOP on it
//     would freeze in-flight streams, and the pool is provisioned always-on
//     anyway (LifecycleConfig{AlwaysUp: true, Hibernate: false} on the host
//     side), so the override is defense-in-depth for a future operator arming
//     Hibernate on a crew pool — the no-op childproc.go anticipated. The
//     no-op is semantically safe because the claim-never-frozen invariant
//     (D9.1) only ever hibernates IDLE processes.
//
// Like adapters #1–#4 it lands as a NEW ADAPTER ONLY: the gateway, router,
// lifecycle and supervisor keep speaking RuntimeAdapter and never see
// "crewai". That zero-control-plane property is the standing acceptance test
// the opencode/codex seam established (P2), vibe re-proved (P4.1) and goose
// re-proved (P4.2); a fifth runtime — a non-CLI, framework-class one —
// exercising it with no control-plane diff is the P4.3 proof, and the first
// evidence the seam generalizes past coding agents.
//
// host is the substrate the process runs on (LocalHost for the prototype; the
// k8squad sandbox-pod host whose image carries python + uv + the pinned
// crewai[a2a] extra later). spec.CLIVersion pins the crewai package version.
func NewCrewAdapter(host PlatformHost, spec RuntimeSpec, opts ...Option) (RuntimeAdapter, error) {
	if host == nil {
		return nil, fmt.Errorf("mesh: NewCrewAdapter requires a PlatformHost")
	}
	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("mesh: crewai runtime spec: %w", err)
	}
	// The crewai wire router is the default transport (Decision B); an
	// explicit WithRouter in opts still wins — it is appended after this
	// default, and options apply in order.
	opts = append([]Option{WithRouter(NewCrewaiRouter(nil, ""))}, opts...)
	return &crewAdapter{
		ChildProcessAdapter: newChildProcessAdapter(
			host,
			spec.Type,
			crewCard(spec),
			crewLaunch(spec),
			opts...,
		),
	}, nil
}

// crewAdapter is adapter #5: the ChildProcessAdapter base with cooperative
// Hibernate/Wake overrides (Decision A). Everything else — Spawn, Register,
// Route delivery, Stop, Recreate — is the inherited base behavior.
type crewAdapter struct {
	*ChildProcessAdapter
}

// Hibernate is the cooperative no-op override: crews never freeze. It emits
// the lifecycle telemetry (op "hibernate.noop") so an operator who armed
// Hibernate on a crew pool sees the refusal instead of silence.
func (a *crewAdapter) Hibernate(ctx context.Context, h Handle) error {
	ch, err := a.recover(h)
	if err != nil {
		return err
	}
	a.sink().OnLifecycle("hibernate.noop", ch.ID(), ch.Endpoint(), nil)
	return nil
}

// Wake is the cooperative no-op override, the symmetric twin of Hibernate: a
// never-frozen process needs no resume, and SIGCONT-ing the asyncio server
// would be a no-op the process never asked for. Telemetry op "wake.noop".
func (a *crewAdapter) Wake(ctx context.Context, h Handle) error {
	ch, err := a.recover(h)
	if err != nil {
		return err
	}
	a.sink().OnLifecycle("wake.noop", ch.ID(), ch.Endpoint(), nil)
	return nil
}

var _ RuntimeAdapter = (*crewAdapter)(nil)

// crewCard returns the agent-card builder for crewai. The shape is identical
// to adapters #1–#4 by design — the card is a pure function of the runtime
// spec plus the envelope, sourced from the RuntimeSpec so the mesh never
// advertises what the crew cannot serve. The crew's own Google agent card
// (served at /.well-known/agent-card.json) is upstream metadata the registry
// stores opaquely; the card announced at Register stays module-shaped with
// the honest initial capability set the design locked: streaming (SSE
// status/artifact/message frames), BYO model endpoint (the litellm lane), NO
// interactivePrompt and NO tool-streaming — crews emit agent-level progress
// only, and there is no module EventTool surface on the Google wire.
func crewCard(spec RuntimeSpec) func(Envelope) a2a.AgentCard {
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

// crewLaunch returns the LaunchSpec builder for crewai. The standing process
// is the crew's A2A server on the host's substrate (the shim supervisor with
// the crewai flavor stamped into its env on LocalHost — the k8squad host's
// supervisor lazily builds the crew server the way it lazily builds its
// coding engines — or the crew entrypoint directly on a purpose-built host).
// The module never supplies the binary: LaunchSpec carries env + dirs, and
// the host owns the exec shape, exactly as for adapters #1–#4.
//
// Runtime-specific halves riding the standing-process env:
//
//   - spec.RuntimeTypeEnv=spec.Type — the flavor stamp, the shared contract.
//   - A BYO OpenAI-compatible ModelRoute rides the litellm lane
//     (OPENAI_BASE_URL/OPENAI_API_KEY), with the zero-credential placeholder
//     on tokenless Ollama-style lanes, mirroring the goose treatment.
//   - AUTH_TOKEN passes through from the envelope env (the host's resolved
//     SimpleTokenAuth secret); the adapter never invents a token.
//
// Caller-supplied env always wins, and the env is built deterministically
// (sorted) so a given envelope yields a byte-identical spec — the property
// Recreate relies on for its bare-exec rebuild. The crew project directory is
// the envelope WorkDir (the crew's own code), with the per-process source
// layout overriding it like every other adapter when Envelope.Source is set.
func crewLaunch(spec RuntimeSpec) func(Envelope) (LaunchSpec, error) {
	return func(env Envelope) (LaunchSpec, error) {
		procEnv := map[string]string{
			spec.RuntimeTypeEnv: spec.Type,
		}
		if env.ModelRoute.Endpoint != "" {
			procEnv[crewOpenAIBaseURLEnv] = env.ModelRoute.Endpoint
			key := env.ModelRoute.Token
			if key == "" {
				key = "ollama" // the zero-credential Ollama lane placeholder
			}
			procEnv[crewOpenAIAPIKeyEnv] = key
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
