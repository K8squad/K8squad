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
	"reflect"
	"strings"
	"testing"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// testVibeType is the runtime flavor name the module tests inject for vibe
// (a host injects its own constant; k8squad host: api/v1alpha1 RuntimeTypeVibe).
const testVibeType = "vibe"

// testVibeSpec is the injected runtime profile standing in for a host's
// declarative vibe runtime — plain values, exactly what a host plugin supplies
// at wiring time (k8squad host: pkg/shim/runtimes + internal/protocol + its
// SchemaVersion stamp). The credential shape is MISTRAL_API_KEY and the pinned
// CLIVersion mirrors the host's reproducibility stamp.
func testVibeSpec() RuntimeSpec {
	return RuntimeSpec{
		Type:           testVibeType,
		CLIVersion:     "py-v0.3.0",
		Capabilities:   a2a.Capabilities{Streaming: true, ToolCalls: true, InteractivePrompt: false, BYOModelEndpoint: true, ArtifactKinds: []string{"file", "patch"}, Docker: true, GitHub: true, PackageInstall: true},
		DefaultModel:   a2a.ModelInfo{ID: "mistral-large-latest", ContextWindow: 131072},
		CredentialType: "mistral_api_key",
		SchemaVersion:  a2a.DefaultSchemaVersion,
		Protocol:       a2a.ProtocolVersions{A2A: "0.2.0", MCP: "2025-06-18"},
		RuntimeTypeEnv: testRuntimeTypeEnv,
	}
}

func TestNewVibeAdapterRequiresHost(t *testing.T) {
	if _, err := NewVibeAdapter(nil, testVibeSpec()); err == nil {
		t.Fatal("NewVibeAdapter must reject a nil host")
	}
}

// The injected-spec seam fails loudly at wiring time (the injected analogue of
// the host registry's "runtime not registered"), never mid-Spawn.
func TestNewVibeAdapterRequiresValidSpec(t *testing.T) {
	spec := testVibeSpec()
	spec.Type = ""
	if _, err := NewVibeAdapter(&fakeHost{}, spec); err == nil {
		t.Fatal("NewVibeAdapter must reject an empty runtime type")
	}
	spec = testVibeSpec()
	spec.RuntimeTypeEnv = ""
	if _, err := NewVibeAdapter(&fakeHost{}, spec); err == nil {
		t.Fatal("NewVibeAdapter must reject an empty launch env contract")
	}
}

func TestVibeSpawnStampsRuntimeAndCard(t *testing.T) {
	host := &fakeHost{}
	spec := testVibeSpec()
	a, err := NewVibeAdapter(host, spec)
	if err != nil {
		t.Fatalf("NewVibeAdapter: %v", err)
	}

	_, card, err := a.Spawn(context.Background(), Envelope{
		Skills:   []string{"code-review"},
		Identity: a2a.AgentIdentity{Name: "coder", Squad: "eng", Project: "k8squad"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if len(host.specs) != 1 {
		t.Fatalf("expected one launch, got %d", len(host.specs))
	}
	if host.specs[0].RuntimeType != spec.Type {
		t.Fatalf("launch runtime type = %q, want %q", host.specs[0].RuntimeType, spec.Type)
	}
	if !hasEnv(host.specs[0].Env, spec.RuntimeTypeEnv+"="+spec.Type) {
		t.Fatalf("launch env %v missing %s=%s", host.specs[0].Env, spec.RuntimeTypeEnv, spec.Type)
	}
	// The headless GrowthBook mitigation: the standing process is pinned to the
	// runtime default model from the start when the envelope carries no override.
	if !hasEnv(host.specs[0].Env, vibeActiveModelEnv+"="+spec.DefaultModel.ID) {
		t.Fatalf("launch env %v missing %s=%s (default-model pin)", host.specs[0].Env, vibeActiveModelEnv, spec.DefaultModel.ID)
	}

	// The card must mirror the injected runtime spec's honest contract.
	if card.Runtime.Type != spec.Type {
		t.Fatalf("card runtime type = %q, want %q", card.Runtime.Type, spec.Type)
	}
	if card.Runtime.CLIVersion != spec.CLIVersion {
		t.Fatalf("card CLI version = %q, want %q", card.Runtime.CLIVersion, spec.CLIVersion)
	}
	if card.Model.ID != spec.DefaultModel.ID {
		t.Fatalf("card model = %q, want default %q", card.Model.ID, spec.DefaultModel.ID)
	}
	if !reflect.DeepEqual(card.Capabilities, spec.Capabilities) {
		t.Fatalf("card capabilities = %+v, want %+v", card.Capabilities, spec.Capabilities)
	}
	if card.Auth.Type != spec.CredentialType {
		t.Fatalf("card auth shape = %q, want %q", card.Auth.Type, spec.CredentialType)
	}
	if card.Agent.Name != "coder" || card.Agent.Project != "k8squad" {
		t.Fatalf("card identity = %+v, want coder/k8squad", card.Agent)
	}
	if card.SchemaVersion != spec.SchemaVersion {
		t.Fatalf("card schema = %q, want %q", card.SchemaVersion, spec.SchemaVersion)
	}
	if card.Protocol != spec.Protocol {
		t.Fatalf("card protocol = %+v, want %+v", card.Protocol, spec.Protocol)
	}
}

func TestVibeModelOverridePinsActiveModel(t *testing.T) {
	host := &fakeHost{}
	spec := testVibeSpec()
	a, _ := NewVibeAdapter(host, spec)
	_, card, err := a.Spawn(context.Background(), Envelope{Model: "mistral-medium-latest"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if card.Model.ID != "mistral-medium-latest" {
		t.Fatalf("card model = %q, want the override mistral-medium-latest", card.Model.ID)
	}
	// The context window stays the runtime's authority, never the override's.
	if card.Model.ContextWindow != spec.DefaultModel.ContextWindow {
		t.Fatalf("context window = %d, want runtime default %d", card.Model.ContextWindow, spec.DefaultModel.ContextWindow)
	}
	// The override must also pin the standing process's active model so the
	// headless GrowthBook bug cannot drop it back to a flag default.
	if !hasEnv(host.specs[0].Env, vibeActiveModelEnv+"=mistral-medium-latest") {
		t.Fatalf("launch env %v missing %s override pin", host.specs[0].Env, vibeActiveModelEnv)
	}
}

func TestVibeBYOModelRouteEnv(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewVibeAdapter(host, testVibeSpec())
	_, _, err := a.Spawn(context.Background(), Envelope{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "qwen3"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	env := host.specs[0].Env
	if !hasEnv(env, "MISTRAL_BASE_URL=http://ollama:11434/v1") {
		t.Fatalf("env %v missing MISTRAL_BASE_URL", env)
	}
	// The zero-credential Ollama lane gets the conventional placeholder key on
	// vibe's native credential shape.
	if !hasEnv(env, "MISTRAL_API_KEY=ollama") {
		t.Fatalf("env %v missing the ollama placeholder key", env)
	}
}

func TestVibeEnvOverrideWins(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewVibeAdapter(host, testVibeSpec())
	_, _, err := a.Spawn(context.Background(), Envelope{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://e/v1"},
		Env:        map[string]string{"MISTRAL_API_KEY": "sk-real"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	env := host.specs[0].Env
	if !hasEnv(env, "MISTRAL_API_KEY=sk-real") {
		t.Fatalf("caller env must override the placeholder; got %v", env)
	}
	if hasEnv(env, "MISTRAL_API_KEY=ollama") {
		t.Fatalf("placeholder key must not survive a caller override; got %v", env)
	}
}

func TestVibeLaunchEnvDeterministic(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewVibeAdapter(host, testVibeSpec())
	env := Envelope{Env: map[string]string{"B": "2", "A": "1"}}
	if _, _, err := a.Spawn(context.Background(), env); err != nil {
		t.Fatalf("Spawn 1: %v", err)
	}
	if _, _, err := a.Spawn(context.Background(), env); err != nil {
		t.Fatalf("Spawn 2: %v", err)
	}
	if strings.Join(host.specs[0].Env, ",") != strings.Join(host.specs[1].Env, ",") {
		t.Fatalf("the same envelope must yield a byte-identical spec: %v vs %v", host.specs[0].Env, host.specs[1].Env)
	}
}

// The vibe adapter must be usable purely through the RuntimeAdapter seam — the
// gateway never sees a concrete type (the "zero control-plane change" test that
// a third runtime exercises at P4.1).
func TestVibeIsRuntimeAdapter(t *testing.T) {
	if _, err := NewVibeAdapter(&fakeHost{}, testVibeSpec()); err != nil {
		t.Fatalf("NewVibeAdapter: %v", err)
	}
}
