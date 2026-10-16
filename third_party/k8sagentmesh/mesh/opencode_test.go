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

// testRuntimeTypeEnv is the launch env contract the module tests inject: the
// env var the standing process's supervisor reads to pick its baked-in flavor.
// A host injects its own supervisor's contract (k8squad host:
// KSQUAD_RUNTIME_TYPE, read by cmd/shim/supervisor.go runtime()); the module
// never hardcodes it (leak L1).
const testRuntimeTypeEnv = "MESH_RUNTIME_TYPE"

// testOpenCodeSpec is the injected runtime profile standing in for a host's
// declarative opencode runtime — plain values, exactly what a host plugin
// supplies at wiring time (k8squad host: pkg/shim/runtimes +
// internal/protocol + its SchemaVersion stamp). The adapter tests assert the
// card and launch env are pure functions of this spec plus the envelope.
func testOpenCodeSpec() RuntimeSpec {
	return RuntimeSpec{
		Type:           "opencode",
		CLIVersion:     "v1.18.27",
		Capabilities:   a2a.Capabilities{Streaming: true, ToolCalls: true, InteractivePrompt: true, BYOModelEndpoint: true, RateLimitSignal: true, ArtifactKinds: []string{"file", "patch"}, Docker: true, GitHub: true},
		DefaultModel:   a2a.ModelInfo{ID: "claude-sonnet-4", ContextWindow: 200000},
		CredentialType: "apikey",
		SchemaVersion:  a2a.DefaultSchemaVersion,
		Protocol:       a2a.ProtocolVersions{A2A: "0.2.0", MCP: "2025-06-18"},
		RuntimeTypeEnv: testRuntimeTypeEnv,
	}
}

func TestNewOpenCodeAdapterRequiresHost(t *testing.T) {
	if _, err := NewOpenCodeAdapter(nil, testOpenCodeSpec()); err == nil {
		t.Fatal("NewOpenCodeAdapter must reject a nil host")
	}
}

// The injected-spec seam fails loudly at wiring time (the injected analogue of
// the host registry's "runtime not registered"), never mid-Spawn.
func TestNewOpenCodeAdapterRequiresValidSpec(t *testing.T) {
	spec := testOpenCodeSpec()
	spec.Type = ""
	if _, err := NewOpenCodeAdapter(&fakeHost{}, spec); err == nil {
		t.Fatal("NewOpenCodeAdapter must reject an empty runtime type")
	}
	spec = testOpenCodeSpec()
	spec.RuntimeTypeEnv = ""
	if _, err := NewOpenCodeAdapter(&fakeHost{}, spec); err == nil {
		t.Fatal("NewOpenCodeAdapter must reject an empty launch env contract")
	}
}

func TestOpenCodeSpawnStampsRuntimeAndCard(t *testing.T) {
	host := &fakeHost{}
	spec := testOpenCodeSpec()
	a, err := NewOpenCodeAdapter(host, spec)
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter: %v", err)
	}

	_, card, err := a.Spawn(context.Background(), Envelope{
		Skills:   []string{"code-review"},
		Identity: a2a.AgentIdentity{Name: "coder", Squad: "eng", Project: "k8squad"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// The launched process is told to be opencode via the injected env
	// contract — which is exactly how the standing process's supervisor picks
	// its baked-in flavor.
	if len(host.specs) != 1 {
		t.Fatalf("expected one launch, got %d", len(host.specs))
	}
	if host.specs[0].RuntimeType != spec.Type {
		t.Fatalf("launch runtime type = %q, want %q", host.specs[0].RuntimeType, spec.Type)
	}
	if !hasEnv(host.specs[0].Env, spec.RuntimeTypeEnv+"="+spec.Type) {
		t.Fatalf("launch env %v missing %s=%s", host.specs[0].Env, spec.RuntimeTypeEnv, spec.Type)
	}

	// The card must mirror the injected runtime spec's honest contract — the
	// spec is the one source of truth, so the mesh never advertises what the
	// process cannot do.
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

// An empty SchemaVersion in the spec falls back to the module default — the
// host-pluggable branding seam (leak L2's counterpart on the adapter side).
func TestOpenCodeCardSchemaDefaultsToModuleDefault(t *testing.T) {
	spec := testOpenCodeSpec()
	spec.SchemaVersion = ""
	a, err := NewOpenCodeAdapter(&fakeHost{}, spec)
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter: %v", err)
	}
	_, card, err := a.Spawn(context.Background(), Envelope{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if card.SchemaVersion != a2a.DefaultSchemaVersion {
		t.Fatalf("card schema = %q, want the module default %q", card.SchemaVersion, a2a.DefaultSchemaVersion)
	}
}

func TestOpenCodeModelOverride(t *testing.T) {
	host := &fakeHost{}
	spec := testOpenCodeSpec()
	a, _ := NewOpenCodeAdapter(host, spec)
	_, card, err := a.Spawn(context.Background(), Envelope{Model: "claude-opus-4"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if card.Model.ID != "claude-opus-4" {
		t.Fatalf("card model = %q, want the override claude-opus-4", card.Model.ID)
	}
	// The context window stays the runtime's authority, never the override's.
	if card.Model.ContextWindow != spec.DefaultModel.ContextWindow {
		t.Fatalf("context window = %d, want runtime default %d", card.Model.ContextWindow, spec.DefaultModel.ContextWindow)
	}
}

func TestOpenCodeBYOModelRouteEnv(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewOpenCodeAdapter(host, testOpenCodeSpec())
	_, _, err := a.Spawn(context.Background(), Envelope{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "qwen3"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	env := host.specs[0].Env
	if !hasEnv(env, "OPENAI_BASE_URL=http://ollama:11434/v1") {
		t.Fatalf("env %v missing OPENAI_BASE_URL", env)
	}
	// The zero-credential Ollama lane gets the conventional placeholder key.
	if !hasEnv(env, "OPENAI_API_KEY=ollama") {
		t.Fatalf("env %v missing the ollama placeholder key", env)
	}
}

func TestOpenCodeEnvOverrideWins(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewOpenCodeAdapter(host, testOpenCodeSpec())
	_, _, err := a.Spawn(context.Background(), Envelope{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://e/v1"},
		Env:        map[string]string{"OPENAI_API_KEY": "sk-real"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	env := host.specs[0].Env
	if !hasEnv(env, "OPENAI_API_KEY=sk-real") {
		t.Fatalf("caller env must override the placeholder; got %v", env)
	}
	if hasEnv(env, "OPENAI_API_KEY=ollama") {
		t.Fatalf("placeholder key must not survive a caller override; got %v", env)
	}
}

func TestOpenCodeLaunchEnvDeterministic(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewOpenCodeAdapter(host, testOpenCodeSpec())
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

// The opencode adapter must be usable purely through the RuntimeAdapter seam —
// the gateway never sees a concrete type (the "zero control-plane change at P2"
// test). The package-level var in opencode.go is the compile-time assertion;
// this test verifies the constructor succeeds.
func TestOpenCodeIsRuntimeAdapter(t *testing.T) {
	if _, err := NewOpenCodeAdapter(&fakeHost{}, testOpenCodeSpec()); err != nil {
		t.Fatalf("NewOpenCodeAdapter: %v", err)
	}
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
