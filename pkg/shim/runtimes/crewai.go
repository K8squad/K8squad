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
	"fmt"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
)

// Single-sourced crewai constants (ADR-017 reproducibility; ISI-6569).
const (
	// crewaiCLIVersion pins the crewai[a2a] release baked into the sandbox
	// image (Dockerfile.toolchain-crewai). The a2a extra drags
	// a2a-sdk ~=0.3.10 (Google A2A 0.3.0) - the wire the crewaiRouter
	// speaks. Bumped here + image in lockstep.
	crewaiCLIVersion = "py-1.15.28"
	// crewaiDefaultModel is the litellm model id the runtime serves when
	// the Agent pins none and no BYO endpoint route is set. CrewAI calls
	// models through litellm, so any litellm-resolvable id
	// (Agent.spec.model / BYO route) overrides it.
	crewaiDefaultModel = "gpt-4.1-mini"
	// crewaiContextWindow is the default model's context-window budget
	// authority for the context Assembler (spec 6.2).
	crewaiContextWindow = 128000
)

// crewAI is the CrewAI multi-agent flavor (ISI-6569, P4.3) - an
// always-on-pool runtime, not a one-shot CLI engine. The standing process is
// the crew project's A2A server (crewai[a2a]: JSON-RPC at /a2a behind
// SimpleTokenAuth reading AUTH_TOKEN); tasks reach it through the module's
// crewaiRouter translation lane, so the per-task Command seam of the one-shot
// engine is deliberately unimplemented: it fails closed pointing at the
// router lane rather than fabricating a launch the mesh adapter owns.
type crewAI struct{}

func (crewAI) Type() string                     { return apiv1alpha1.RuntimeTypeCrewAI }
func (crewAI) CLIVersion() string               { return crewaiCLIVersion }
func (crewAI) CredentialShape() CredentialShape { return ShapeAPIKey }

// Capabilities is the honest CrewAI set (ISI-6569): the a2a lane streams
// (SSE on message/stream), BYO model endpoints ride litellm's
// OpenAI-compatible pair (OPENAI_BASE_URL / OPENAI_API_KEY), and crews
// expose neither interactive prompts nor bare tool-call turns on the A2A
// wire - crew activity arrives as task / status-update / artifact-update
// frames the crewaiRouter maps onto the module wire.
func (crewAI) Capabilities() a2a.Capabilities {
	return a2a.Capabilities{
		Streaming:         true,
		ToolCalls:         false, // no tool-call turns on the A2A wire
		InteractivePrompt: false, // batch orchestration; no input-required turn
		BYOModelEndpoint:  true,  // litellm lane: OPENAI_BASE_URL / OPENAI_API_KEY
		RouterServed:      true,  // tasks reach the standing crew A2A server via crewaiRouter; no one-shot engine launch
		ArtifactKinds:     []string{"text"},
		Docker:            true,
		GitHub:            true,
		PackageInstall:    true, // uv/Python toolchain in the sandbox image
	}
}

func (crewAI) DefaultModel() a2a.ModelInfo {
	return a2a.ModelInfo{ID: crewaiDefaultModel, ContextWindow: crewaiContextWindow}
}

// Command fails closed: CrewAI is router-served (ISI-6569). A crew task is
// routed from the mesh through the adapter's crewaiRouter onto the standing
// crew A2A server the ChildProcessAdapter lane spawns; the one-shot shim
// engine lane (one process per routed task) has no crewai invocation to
// build, so an honest error beats a fabricated launch. The credential and
// BYO endpoint mapping for the standing process is owned by the module-side
// launch: OPENAI_API_KEY / OPENAI_BASE_URL for the litellm lane plus
// AUTH_TOKEN for the server's SimpleTokenAuth.
func (crewAI) Command(LaunchContext) (ExecSpec, error) {
	return ExecSpec{}, fmt.Errorf("runtimes/crewai: router-served runtime - tasks reach the standing crew A2A server via the mesh crewaiRouter lane; the one-shot engine lane does not launch crewai")
}

func init() { Register(crewAI{}) }
