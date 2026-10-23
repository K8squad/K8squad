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
	"strings"
	"testing"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
)

// TestCrewAICard asserts the declarative registry entry (ISI-6569): type
// keyed on the conformant constant, pinned CLI revision, API-key credential
// shape (litellm OPENAI_API_KEY lane) and the honest capability set - the
// gaps the mesh card must not hide.
func TestCrewAICard(t *testing.T) {
	rt, err := Get(apiv1alpha1.RuntimeTypeCrewAI)
	if err != nil {
		t.Fatalf("Get(crewai) failed: %v", err)
	}
	if rt.Type() != apiv1alpha1.RuntimeTypeCrewAI {
		t.Errorf("Type() = %q, want %q", rt.Type(), apiv1alpha1.RuntimeTypeCrewAI)
	}
	if rt.CLIVersion() != crewaiCLIVersion {
		t.Errorf("CLIVersion() = %q, want %q", rt.CLIVersion(), crewaiCLIVersion)
	}
	if rt.CredentialShape() != ShapeAPIKey {
		t.Errorf("CredentialShape() = %q, want %q", rt.CredentialShape(), ShapeAPIKey)
	}
	caps := rt.Capabilities()
	if !caps.Streaming {
		t.Error("crewai must advertise streaming=true (a2a message/stream SSE)")
	}
	if !caps.BYOModelEndpoint {
		t.Error("crewai must advertise byoModelEndpoint=true (litellm OpenAI-compatible lane)")
	}
	if caps.ToolCalls {
		t.Error("crewai must advertise toolCalls=false (no tool-call turns on the A2A wire)")
	}
	if caps.InteractivePrompt {
		t.Error("crewai must advertise interactivePrompt=false (batch orchestration)")
	}
	if caps.ArtifactKinds == nil {
		t.Error("crewai must declare artifactKinds, not nil")
	}
	m := rt.DefaultModel()
	if m.ID == "" || m.ContextWindow <= 0 {
		t.Errorf("DefaultModel() = %+v, want a resolvable id + window", m)
	}
}

// TestCrewAICommandFailsClosed pins the router-served posture (ISI-6569):
// the one-shot engine lane refuses to fabricate a crewai launch, and the
// refusal names the lane that owns it.
func TestCrewAICommandFailsClosed(t *testing.T) {
	rt, err := Get(apiv1alpha1.RuntimeTypeCrewAI)
	if err != nil {
		t.Fatalf("Get(crewai) failed: %v", err)
	}
	spec, err := rt.Command(LaunchContext{
		Envelope:   a2a.Envelope{Input: "chart the fleet"},
		Credential: "super-secret-token-value",
		Model:      "some-model",
	})
	if err == nil {
		t.Fatalf("Command must fail closed for the router-served crewai runtime; got spec %+v", spec)
	}
	if !strings.Contains(err.Error(), "crewaiRouter") {
		t.Errorf("Command error must point at the mesh crewaiRouter lane; got %q", err.Error())
	}
}
