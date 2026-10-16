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

package mesh

import (
	"fmt"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/protocol"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

// NewCodexAdapter builds adapter #2 (chapter §9.3 P2, ISI-5826): ChatGPT Codex
// behind the same 7-op RuntimeAdapter seam. It lands as a NEW ADAPTER ONLY —
// no control-plane line changes: the gateway, router, lifecycle and supervisor
// keep speaking RuntimeAdapter and never see "codex". That zero-diff property
// IS the P2 acceptance test — the proof that the seam opencode exercised at
// P1 generalizes to a second runtime, before P3 lifts the [generic] surfaces
// out.
//
// Like adapter #1, Codex runs as a standing child process (`shim supervisor`
// with KSQUAD_RUNTIME_TYPE=codex) on the shared ChildProcessAdapter base, so
// Spawn/Route/Stop/Recreate map onto the shim and Hibernate/Wake are the
// inherited SIGSTOP/SIGCONT base. The runtime-specific halves — the launch env
// and the agent card — are sourced from the existing declarative codex
// adapter (pkg/shim/runtimes), keeping one source of truth for the capability
// contract the mesh announces.
//
// host is the substrate the process runs on (LocalHost for the prototype, a
// kube sandbox-pod host later).
func NewCodexAdapter(host PlatformHost, opts ...Option) (RuntimeAdapter, error) {
	if host == nil {
		return nil, fmt.Errorf("mesh: NewCodexAdapter requires a PlatformHost")
	}
	rt, err := runtimes.Get(apiv1alpha1.RuntimeTypeCodex)
	if err != nil {
		return nil, fmt.Errorf("mesh: codex runtime not registered: %w", err)
	}
	return newChildProcessAdapter(
		host,
		apiv1alpha1.RuntimeTypeCodex,
		codexCard(rt),
		codexLaunch(rt),
		opts...,
	), nil
}

// codexCard returns the agent-card builder for codex: it mirrors the shim
// engine's own card construction, sourcing the honest capability set, default
// model and credential shape from the declarative runtime so the mesh never
// advertises what the process cannot serve. The shape is identical to adapter
// #1's builder by design — the card is a pure function of the declarative
// runtime plus the envelope, and codex differs from opencode only in the
// values that runtime supplies (CLI pin, default model, capabilities).
func codexCard(rt runtimes.Runtime) func(Envelope) a2a.AgentCard {
	return func(env Envelope) a2a.AgentCard {
		model := rt.DefaultModel()
		if env.Model != "" {
			model.ID = env.Model // context window stays the runtime's authority
		}
		skills := env.Skills
		if skills == nil {
			skills = []string{}
		}
		return a2a.AgentCard{
			SchemaVersion: a2a.SchemaVersion,
			Agent: a2a.AgentIdentity{
				Name:    env.Identity.Name,
				Squad:   env.Identity.Squad,
				Project: env.Identity.Project,
			},
			Runtime: a2a.RuntimeInfo{
				Type:        rt.Type(),
				CLIVersion:  rt.CLIVersion(),
				ShimVersion: shimVersion,
			},
			Model:  model,
			Skills: skills,
			Auth: a2a.AuthInfo{
				Type: string(rt.CredentialShape()),
			},
			Capabilities: rt.Capabilities(),
			Protocol:     protocol.Pinned(),
		}
	}
}

// codexLaunch returns the LaunchSpec builder for codex. The standing process
// is `shim supervisor` with the flavor stamped into its env — the supervisor
// lazily builds its codex engine on the first routed task, exactly as for
// opencode. A BYO model route rides the OpenAI-compatible env the codex
// runtime honors natively (it speaks the OpenAI wire; the shim's per-task
// codex Command maps the same route onto OPENAI_BASE_URL/OPENAI_API_KEY plus a
// config.toml model_providers superset), so the endpoint must be present on
// the process from the start. Caller-supplied env wins over the defaults, and
// the env is built deterministically (sorted) so a given envelope yields a
// byte-identical spec — the property Recreate relies on for its bare-exec
// rebuild. The per-process source layout (ADR-0024 §1) overrides the plain
// WorkDir the same way it does for adapter #1.
func codexLaunch(rt runtimes.Runtime) func(Envelope) (LaunchSpec, error) {
	return func(env Envelope) (LaunchSpec, error) {
		procEnv := map[string]string{
			envRuntimeType: rt.Type(),
		}
		if env.ModelRoute.Endpoint != "" {
			procEnv["OPENAI_BASE_URL"] = env.ModelRoute.Endpoint
			key := env.ModelRoute.Token
			if key == "" {
				key = "ollama" // the zero-credential Ollama lane placeholder
			}
			procEnv["OPENAI_API_KEY"] = key
		}
		for k, v := range env.Env {
			procEnv[k] = v
		}
		spec := LaunchSpec{
			RuntimeType: rt.Type(),
			Env:         sortedEnv(procEnv),
			WorkDir:     env.WorkDir,
			Identity:    env.Identity,
			Source:      env.Source,
		}
		if env.Source != nil {
			spec.WorkDir = env.Source.WorktreeDir
		}
		return spec, nil
	}
}
