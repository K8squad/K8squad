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
	"sort"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/protocol"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

// envRuntimeType is the env var the shim supervisor reads to pick its baked-in
// runtime flavor (cmd/shim/supervisor.go runtime(): KSQUAD_RUNTIME_TYPE). Spawn
// stamps it so the standing process serves opencode.
const envRuntimeType = "KSQUAD_RUNTIME_TYPE"

// shimVersion is the mesh-side shim revision stamped on the agent card. The
// standing process serves its own card over A2A at runtime; this is the
// envelope-time card the gateway registers before the first route, so it mirrors
// the shim's own default rather than claiming a pinned build it cannot see.
const shimVersion = "mesh-p1"

// NewOpenCodeAdapter builds adapter #1 (chapter §1B, D8.1): opencode wrapped
// behind the 7-op RuntimeAdapter. It is a ChildProcessAdapter — opencode runs as
// a standing child process (`shim supervisor` with KSQUAD_RUNTIME_TYPE=opencode),
// so Spawn/Route/Stop/Recreate map onto the existing shim and Hibernate/Wake are
// the inherited SIGSTOP/SIGCONT base. The runtime-specific halves — the launch
// env and the agent card — are sourced from the existing declarative opencode
// adapter (pkg/shim/runtimes), so the capability contract the mesh announces is
// exactly the one the shim serves; there is one source of truth, not two.
//
// host is the substrate the process runs on (LocalHost for the prototype, a
// kube sandbox-pod host later). The returned adapter speaks only RuntimeAdapter;
// the gateway/supervisor never see "opencode" — which is the test that adding a
// second runtime at P2 is a new adapter with zero control-plane change.
func NewOpenCodeAdapter(host PlatformHost, opts ...Option) (RuntimeAdapter, error) {
	if host == nil {
		return nil, fmt.Errorf("mesh: NewOpenCodeAdapter requires a PlatformHost")
	}
	rt, err := runtimes.Get(apiv1alpha1.RuntimeTypeOpenCode)
	if err != nil {
		return nil, fmt.Errorf("mesh: opencode runtime not registered: %w", err)
	}
	return newChildProcessAdapter(
		host,
		apiv1alpha1.RuntimeTypeOpenCode,
		openCodeCard(rt),
		openCodeLaunch(rt),
		opts...,
	), nil
}

// openCodeCard returns the agent-card builder for opencode: it mirrors the
// shim engine's own card construction (pkg/shim/engine.go AgentCard), sourcing
// the honest capability set, default model and credential shape from the
// declarative runtime so the mesh never drifts from what the process serves.
func openCodeCard(rt runtimes.Runtime) func(Envelope) a2a.AgentCard {
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

// openCodeLaunch returns the LaunchSpec builder for opencode. The standing
// process is `shim supervisor` with the flavor stamped into its env; the
// supervisor lazily builds its opencode engine on the first routed task
// (cmd/shim/supervisor.go runtime()). The envelope's model route and extra env
// (a resolved credential) are layered on so the process serves the right model
// before any task arrives — the "pre-loaded with the role envelope" half of
// Spawn. The env is built deterministically (sorted) so a given envelope yields
// a byte-identical spec, which keeps Recreate a true bare-exec rebuild.
func openCodeLaunch(rt runtimes.Runtime) func(Envelope) (LaunchSpec, error) {
	return func(env Envelope) (LaunchSpec, error) {
		procEnv := map[string]string{
			envRuntimeType: rt.Type(),
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
		return LaunchSpec{
			RuntimeType: rt.Type(),
			Env:         sortedEnv(procEnv),
			WorkDir:     env.WorkDir,
			Identity:    env.Identity,
		}, nil
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
