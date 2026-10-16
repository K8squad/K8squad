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

// adapters.go — the k8squad host plugin over the module's injected RuntimeSpec
// seam (leak L1 of the ISI-5827 extraction spec): the constructor signatures
// the host has used since P1/P2, with the runtime-specific halves sourced
// from the existing declarative runtime registry (pkg/shim/runtimes), the
// pinned wire revisions (internal/protocol.Pinned) and the ksquad-branded
// Agent Card schema tag — one source of truth per host, never two.
package mesh

import (
	"fmt"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/protocol"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"

	a2amesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// envRuntimeType is the env var the shim supervisor reads to pick its baked-in
// runtime flavor (cmd/shim/supervisor.go runtime(): KSQUAD_RUNTIME_TYPE). It
// is the host's RuntimeTypeEnv contract — the module stamps it via
// RuntimeSpec.RuntimeTypeEnv so the standing process serves the right flavor.
const envRuntimeType = "KSQUAD_RUNTIME_TYPE"

// ksquadRuntimeSpec builds the injected runtime profile for a registered
// k8squad runtime: everything the module's card and launch builders need,
// sourced from the same declarative runtime the shim serves, so the
// capability contract the mesh announces is exactly the one the process
// serves.
func ksquadRuntimeSpec(rt runtimes.Runtime) RuntimeSpec {
	return RuntimeSpec{
		Type:           rt.Type(),
		CLIVersion:     rt.CLIVersion(),
		Capabilities:   rt.Capabilities(),
		DefaultModel:   rt.DefaultModel(),
		CredentialType: string(rt.CredentialShape()),
		SchemaVersion:  a2a.SchemaVersion, // "ksquad.a2a/v1" — host-stamped
		Protocol:       protocol.Pinned(), // pinned A2A wire revisions
		RuntimeTypeEnv: envRuntimeType,
	}
}

// NewOpenCodeAdapter builds adapter #1 (chapter §1B, D8.1): opencode wrapped
// behind the 7-op RuntimeAdapter. It is a ChildProcessAdapter — opencode runs
// as a standing child process (`shim supervisor` with KSQUAD_RUNTIME_TYPE=
// opencode), so Spawn/Route/Stop/Recreate map onto the existing shim and
// Hibernate/Wake are the inherited SIGSTOP/SIGCONT base. The runtime-specific
// halves — the launch env and the agent card — are built by the module from
// the injected RuntimeSpec, so there stays one source of truth: the declarative
// opencode adapter (pkg/shim/runtimes).
//
// host is the substrate the process runs on (LocalHost for the prototype, a
// kube sandbox-pod host later). The returned adapter speaks only RuntimeAdapter;
// the gateway/supervisor never see "opencode".
func NewOpenCodeAdapter(host PlatformHost, opts ...Option) (RuntimeAdapter, error) {
	if host == nil {
		return nil, fmt.Errorf("mesh: NewOpenCodeAdapter requires a PlatformHost")
	}
	rt, err := runtimes.Get(apiv1alpha1.RuntimeTypeOpenCode)
	if err != nil {
		return nil, fmt.Errorf("mesh: opencode runtime not registered: %w", err)
	}
	return a2amesh.NewOpenCodeAdapter(host, ksquadRuntimeSpec(rt), opts...)
}

// NewCodexAdapter builds adapter #2 (chapter §9.3 P2, ISI-5826): ChatGPT Codex
// behind the same 7-op RuntimeAdapter seam. It lands as a NEW ADAPTER ONLY —
// no control-plane line changes: the gateway, router, lifecycle and supervisor
// keep speaking RuntimeAdapter and never see "codex". Like adapter #1, Codex
// runs as a standing child process (`shim supervisor` with
// KSQUAD_RUNTIME_TYPE=codex) on the shared ChildProcessAdapter base.
func NewCodexAdapter(host PlatformHost, opts ...Option) (RuntimeAdapter, error) {
	if host == nil {
		return nil, fmt.Errorf("mesh: NewCodexAdapter requires a PlatformHost")
	}
	rt, err := runtimes.Get(apiv1alpha1.RuntimeTypeCodex)
	if err != nil {
		return nil, fmt.Errorf("mesh: codex runtime not registered: %w", err)
	}
	return a2amesh.NewCodexAdapter(host, ksquadRuntimeSpec(rt), opts...)
}
