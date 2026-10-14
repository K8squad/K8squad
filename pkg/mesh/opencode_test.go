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
	"context"
	"reflect"
	"strings"
	"testing"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

func TestNewOpenCodeAdapterRequiresHost(t *testing.T) {
	if _, err := NewOpenCodeAdapter(nil); err == nil {
		t.Fatal("NewOpenCodeAdapter must reject a nil host")
	}
}

func TestOpenCodeSpawnStampsRuntimeAndCard(t *testing.T) {
	host := &fakeHost{}
	a, err := NewOpenCodeAdapter(host)
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

	// The launched process is told to be opencode via KSQUAD_RUNTIME_TYPE —
	// which is exactly how the shim supervisor picks its baked-in flavor.
	if len(host.specs) != 1 {
		t.Fatalf("expected one launch, got %d", len(host.specs))
	}
	if host.specs[0].RuntimeType != apiv1alpha1.RuntimeTypeOpenCode {
		t.Fatalf("launch runtime type = %q, want opencode", host.specs[0].RuntimeType)
	}
	if !hasEnv(host.specs[0].Env, envRuntimeType+"="+apiv1alpha1.RuntimeTypeOpenCode) {
		t.Fatalf("launch env %v missing %s=opencode", host.specs[0].Env, envRuntimeType)
	}

	// The card must mirror the declarative opencode runtime's honest contract —
	// one source of truth, so the mesh never advertises what the process cannot do.
	rt, _ := runtimes.Get(apiv1alpha1.RuntimeTypeOpenCode)
	if card.Runtime.Type != rt.Type() {
		t.Fatalf("card runtime type = %q, want %q", card.Runtime.Type, rt.Type())
	}
	if card.Runtime.CLIVersion != rt.CLIVersion() {
		t.Fatalf("card CLI version = %q, want %q", card.Runtime.CLIVersion, rt.CLIVersion())
	}
	if card.Model.ID != rt.DefaultModel().ID {
		t.Fatalf("card model = %q, want default %q", card.Model.ID, rt.DefaultModel().ID)
	}
	if !reflect.DeepEqual(card.Capabilities, rt.Capabilities()) {
		t.Fatalf("card capabilities = %+v, want %+v", card.Capabilities, rt.Capabilities())
	}
	if card.Auth.Type != string(rt.CredentialShape()) {
		t.Fatalf("card auth shape = %q, want %q", card.Auth.Type, rt.CredentialShape())
	}
	if card.Agent.Name != "coder" || card.Agent.Project != "k8squad" {
		t.Fatalf("card identity = %+v, want coder/k8squad", card.Agent)
	}
	if card.SchemaVersion != a2a.SchemaVersion {
		t.Fatalf("card schema = %q, want %q", card.SchemaVersion, a2a.SchemaVersion)
	}
}

func TestOpenCodeModelOverride(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewOpenCodeAdapter(host)
	_, card, err := a.Spawn(context.Background(), Envelope{Model: "claude-opus-4"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if card.Model.ID != "claude-opus-4" {
		t.Fatalf("card model = %q, want the override claude-opus-4", card.Model.ID)
	}
	// The context window stays the runtime's authority, never the override's.
	rt, _ := runtimes.Get(apiv1alpha1.RuntimeTypeOpenCode)
	if card.Model.ContextWindow != rt.DefaultModel().ContextWindow {
		t.Fatalf("context window = %d, want runtime default %d", card.Model.ContextWindow, rt.DefaultModel().ContextWindow)
	}
}

func TestOpenCodeBYOModelRouteEnv(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewOpenCodeAdapter(host)
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
	a, _ := NewOpenCodeAdapter(host)
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
	a, _ := NewOpenCodeAdapter(host)
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
// test).
func TestOpenCodeIsRuntimeAdapter(t *testing.T) {
	var _ RuntimeAdapter
	a, err := NewOpenCodeAdapter(&fakeHost{})
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter: %v", err)
	}
	var ra RuntimeAdapter = a
	if ra == nil {
		t.Fatal("opencode adapter must satisfy RuntimeAdapter")
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
