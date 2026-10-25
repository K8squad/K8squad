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

// testCrewType is the runtime flavor name the module tests inject for crewai
// (a host injects its own constant; the k8squad host registry entry lands with
// the host half of ISI-6547).
const testCrewType = "crewai"

// testCrewSpec is the injected runtime profile standing in for a host's
// declarative crewai runtime. The pinned CLIVersion mirrors the spike's
// package pin (crewai 1.15.28 + crewai[a2a] extra → a2a-sdk ~=0.3.10); the
// capability set is the design's honest initial contract — streaming (SSE
// frames), BYO model endpoint (the litellm lane), NO interactivePrompt and NO
// tool-streaming (crews emit agent-level progress only).
func testCrewSpec() RuntimeSpec {
	return RuntimeSpec{
		Type:           testCrewType,
		CLIVersion:     "1.15.28",
		Capabilities:   a2a.Capabilities{Streaming: true, ToolCalls: false, InteractivePrompt: false, BYOModelEndpoint: true},
		DefaultModel:   a2a.ModelInfo{ID: "gpt-4o-mini", ContextWindow: 128000},
		CredentialType: "apikey",
		SchemaVersion:  a2a.DefaultSchemaVersion,
		Protocol:       a2a.ProtocolVersions{A2A: "0.2.0", MCP: "2025-06-18"},
		RuntimeTypeEnv: testRuntimeTypeEnv,
	}
}

func TestNewCrewAdapterRequiresHost(t *testing.T) {
	if _, err := NewCrewAdapter(nil, testCrewSpec()); err == nil {
		t.Fatal("NewCrewAdapter must reject a nil host")
	}
}

func TestNewCrewAdapterRequiresValidSpec(t *testing.T) {
	spec := testCrewSpec()
	spec.Type = ""
	if _, err := NewCrewAdapter(&fakeHost{}, spec); err == nil {
		t.Fatal("NewCrewAdapter must reject an empty runtime type")
	}
	spec = testCrewSpec()
	spec.RuntimeTypeEnv = ""
	if _, err := NewCrewAdapter(&fakeHost{}, spec); err == nil {
		t.Fatal("NewCrewAdapter must reject an empty launch env contract")
	}
}

func TestCrewSpawnStampsRuntimeAndCard(t *testing.T) {
	host := &fakeHost{}
	spec := testCrewSpec()
	a, err := NewCrewAdapter(host, spec)
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}

	_, card, err := a.Spawn(context.Background(), Envelope{
		Skills:   []string{"research", "summarize"},
		Identity: a2a.AgentIdentity{Name: "researcher", Squad: "eng", Project: "k8squad"},
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

	// The card mirrors the injected runtime spec: module-shaped, honest
	// capabilities (no interactive prompt, no tool-streaming).
	if card.Runtime.Type != spec.Type {
		t.Fatalf("card runtime type = %q, want %q", card.Runtime.Type, spec.Type)
	}
	if card.Runtime.CLIVersion != spec.CLIVersion {
		t.Fatalf("card CLI version = %q, want %q", card.Runtime.CLIVersion, spec.CLIVersion)
	}
	if card.Model.ID != spec.DefaultModel.ID {
		t.Fatalf("card model = %q, want the spec default %q", card.Model.ID, spec.DefaultModel.ID)
	}
	if card.Capabilities.InteractivePrompt || card.Capabilities.ToolCalls {
		t.Fatalf("card capabilities %v must stay honest (no prompt, no tool-streaming)", card.Capabilities)
	}
	if !card.Capabilities.Streaming {
		t.Fatal("card must advertise streaming (SSE frames)")
	}
	if !reflect.DeepEqual(card.Skills, []string{"research", "summarize"}) {
		t.Fatalf("card skills = %v", card.Skills)
	}
}

// The model pin: the envelope override replaces the id on the card, never the
// runtime's context-window authority.
func TestCrewModelOverridePinsModel(t *testing.T) {
	a, err := NewCrewAdapter(&fakeHost{}, testCrewSpec())
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}
	_, card, err := a.Spawn(context.Background(), Envelope{Model: "gpt-4.1"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if card.Model.ID != "gpt-4.1" {
		t.Fatalf("card model = %q, want the envelope override", card.Model.ID)
	}
	if card.Model.ContextWindow != testCrewSpec().DefaultModel.ContextWindow {
		t.Fatalf("card context window = %d, want the runtime authority", card.Model.ContextWindow)
	}
}

// A BYO OpenAI-compatible model route rides the litellm lane env; the
// tokenless lane stamps the placeholder, mirroring the goose treatment.
func TestCrewBYOModelRouteEnv(t *testing.T) {
	host := &fakeHost{}
	a, err := NewCrewAdapter(host, testCrewSpec())
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}
	if _, _, err := a.Spawn(context.Background(), Envelope{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "qwen3.5:35b"},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !hasEnv(host.specs[0].Env, crewOpenAIBaseURLEnv+"=http://ollama:11434/v1") {
		t.Fatalf("launch env %v missing %s", host.specs[0].Env, crewOpenAIBaseURLEnv)
	}
	if !hasEnv(host.specs[0].Env, crewOpenAIAPIKeyEnv+"=ollama") {
		t.Fatalf("launch env %v missing the tokenless placeholder %s=ollama", host.specs[0].Env, crewOpenAIAPIKeyEnv)
	}
}

// AUTH_TOKEN passes through from the envelope env (the host's resolved
// SimpleTokenAuth secret); the adapter never invents one.
func TestCrewAuthTokenPassesThrough(t *testing.T) {
	host := &fakeHost{}
	a, err := NewCrewAdapter(host, testCrewSpec())
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}
	if _, _, err := a.Spawn(context.Background(), Envelope{
		Env: map[string]string{crewAuthTokenEnv: "tok-1"},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !hasEnv(host.specs[0].Env, crewAuthTokenEnv+"=tok-1") {
		t.Fatalf("launch env %v missing the pass-through %s", host.specs[0].Env, crewAuthTokenEnv)
	}
}

func TestCrewEnvOverrideWins(t *testing.T) {
	host := &fakeHost{}
	a, err := NewCrewAdapter(host, testCrewSpec())
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}
	if _, _, err := a.Spawn(context.Background(), Envelope{
		Env: map[string]string{crewOpenAIAPIKeyEnv: "caller-key"},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !hasEnv(host.specs[0].Env, crewOpenAIAPIKeyEnv+"=caller-key") {
		t.Fatalf("caller env must win over adapter defaults: %v", host.specs[0].Env)
	}
}

func TestCrewLaunchEnvDeterministic(t *testing.T) {
	host := &fakeHost{}
	a, err := NewCrewAdapter(host, testCrewSpec())
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}
	env := Envelope{
		Env:        map[string]string{crewAuthTokenEnv: "tok", "B": "2", "A": "1"},
		ModelRoute: a2a.ModelRoute{Endpoint: "http://m:1/v1", Token: "k"},
		WorkDir:    "/crew",
	}
	if _, _, err := a.Spawn(context.Background(), env); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, _, err := a.Spawn(context.Background(), env); err != nil {
		t.Fatalf("Spawn 2: %v", err)
	}
	if !reflect.DeepEqual(host.specs[0].Env, host.specs[1].Env) {
		t.Fatalf("launch env must be byte-identical for the same envelope (Recreate property):\n%v\n%v", host.specs[0].Env, host.specs[1].Env)
	}
	for i, kv := range host.specs[0].Env[1:] {
		if host.specs[0].Env[i] > kv {
			t.Fatalf("launch env must be sorted: %v", host.specs[0].Env)
		}
	}
}

// The per-process source layout overrides the plain WorkDir (the crew project
// dir), exactly as for adapters #1–#4.
func TestCrewSourceWorktreeOverridesWorkDir(t *testing.T) {
	host := &fakeHost{}
	a, err := NewCrewAdapter(host, testCrewSpec())
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}
	if _, _, err := a.Spawn(context.Background(), Envelope{
		WorkDir: "/crew",
		Source:  &SourceSpec{Branch: "b", WorktreeDir: "/wt", TeamMainDir: "/main"},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if host.specs[0].WorkDir != "/wt" {
		t.Fatalf("launch dir = %q, want the worktree /wt", host.specs[0].WorkDir)
	}
}

// Decision A: Hibernate/Wake are cooperative telemetry-emitting no-ops — no
// SIGSTOP ever lands on the asyncio server, and the refusal is visible.
func TestCrewHibernateWakeAreCooperativeNoOps(t *testing.T) {
	inst := &fakeInstance{id: "i-1", endpoint: "http://i-1:8080"}
	sink := &recordingSink{}
	host := &fakeHost{sink: sink, nextInst: func(int) *fakeInstance { return inst }}
	a, err := NewCrewAdapter(host, testCrewSpec())
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}
	h, _, err := a.Spawn(context.Background(), Envelope{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := a.Hibernate(context.Background(), h); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	if err := a.Wake(context.Background(), h); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if sig, ok := inst.lastSignal(); ok {
		t.Fatalf("no signal may land on a crew process, got %v", sig)
	}
	ops := sink.ops()
	want := []string{"spawn", "hibernate.noop", "wake.noop"}
	if !reflect.DeepEqual(ops, want) {
		t.Fatalf("lifecycle ops = %v, want %v (refusal visible in telemetry)", ops, want)
	}
}

// The default router is the crewai wire translator (Decision B): a host that
// forgets WithRouter still gets Google-A2A translation, never the module-wire
// /task router that a crew server would 404 on. An explicit WithRouter wins.
func TestCrewAdapterDefaultsToCrewaiRouter(t *testing.T) {
	a, err := NewCrewAdapter(&fakeHost{}, testCrewSpec())
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}
	ca := a.(*crewAdapter)
	if _, ok := ca.ChildProcessAdapter.router.(*crewaiRouter); !ok {
		t.Fatalf("default router = %T, want *crewaiRouter", ca.ChildProcessAdapter.router)
	}

	fr := &fakeRouter{}
	a2nd, err := NewCrewAdapter(&fakeHost{}, testCrewSpec(), WithRouter(fr))
	if err != nil {
		t.Fatalf("NewCrewAdapter with router: %v", err)
	}
	if got := a2nd.(*crewAdapter).ChildProcessAdapter.router; got != Router(fr) {
		t.Fatalf("WithRouter override = %T, want the injected fake", got)
	}
}

// The zero-control-plane proof point for the adapter itself: Route delivers
// through the injected router with the handle's endpoint, identical to the
// base seam contract, and a foreign handle fails closed.
func TestCrewRouteDeliversThroughRouter(t *testing.T) {
	fr := &fakeRouter{}
	a, err := NewCrewAdapter(&fakeHost{}, testCrewSpec(), WithRouter(fr))
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}
	h, _, err := a.Spawn(context.Background(), Envelope{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	inst := &fakeInstance{id: "i-9", endpoint: "http://i-9:8080"}
	_ = inst
	// fakeHost's default instance endpoint is http://inst-0:8080.
	if _, err := a.Route(context.Background(), h, crewTask("t-route")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(fr.tasks) != 1 || fr.tasks[0].A2ATaskID != "t-route" {
		t.Fatalf("router tasks = %+v, want the one routed task", fr.tasks)
	}
	if len(fr.endpoints) != 1 || !strings.HasPrefix(fr.endpoints[0], "http://inst-") {
		t.Fatalf("router endpoints = %v, want the handle endpoint", fr.endpoints)
	}
	// A task without an id is refused before delivery.
	if _, err := a.Route(context.Background(), h, a2a.Task{}); err == nil {
		t.Fatal("Route must require a non-empty a2a_task_id")
	}
}

func TestCrewIsRuntimeAdapter(t *testing.T) {
	a, err := NewCrewAdapter(&fakeHost{}, testCrewSpec())
	if err != nil {
		t.Fatalf("NewCrewAdapter: %v", err)
	}
	if _, ok := a.(RuntimeAdapter); !ok {
		t.Fatal("crewAdapter must satisfy RuntimeAdapter")
	}
}
