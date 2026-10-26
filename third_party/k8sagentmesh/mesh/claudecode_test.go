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
	"testing"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// testClaudeCodeType is the runtime flavor name the module tests inject for
// claude-code (a host injects its own constant; k8squad host: api/v1alpha1
// RuntimeTypeClaudeCode).
const testClaudeCodeType = "claude-code"

// testClaudeCodeSpec is the injected runtime profile standing in for a host's
// declarative claude-code runtime — plain values, exactly what a host plugin
// supplies at wiring time (k8squad host: pkg/shim/runtimes +
// internal/protocol + its SchemaVersion stamp). The pinned CLIVersion, the
// Claude-family default model and the oauth-token credential shape mirror
// the engine's single-sourced constants; the capability set is the honest
// P4.4 surface — streaming, tool calls, NO interactive approval, NO BYO
// OpenAI-compatible endpoint (the CLI speaks the Anthropic wire).
func testClaudeCodeSpec() RuntimeSpec {
	return RuntimeSpec{
		Type:           testClaudeCodeType,
		CLIVersion:     "2.1.239",
		Capabilities:   a2a.Capabilities{Streaming: true, ToolCalls: true, InteractivePrompt: false, BYOModelEndpoint: false},
		DefaultModel:   a2a.ModelInfo{ID: "claude-sonnet-4", ContextWindow: 200000},
		CredentialType: "oauth-token",
		SchemaVersion:  a2a.DefaultSchemaVersion,
		Protocol:       a2a.ProtocolVersions{A2A: "0.2.0", MCP: "2025-06-18"},
		RuntimeTypeEnv: testRuntimeTypeEnv,
	}
}

func TestNewClaudeCodeAdapterRequiresHost(t *testing.T) {
	if _, err := NewClaudeCodeAdapter(nil, testClaudeCodeSpec()); err == nil {
		t.Fatal("NewClaudeCodeAdapter must reject a nil host")
	}
}

// The injected-spec seam fails loudly at wiring time (the injected analogue of
// the host registry's "runtime not registered"), never mid-Spawn.
func TestNewClaudeCodeAdapterRequiresValidSpec(t *testing.T) {
	spec := testClaudeCodeSpec()
	spec.Type = ""
	if _, err := NewClaudeCodeAdapter(&fakeHost{}, spec); err == nil {
		t.Fatal("NewClaudeCodeAdapter must reject an empty runtime type")
	}
	spec = testClaudeCodeSpec()
	spec.RuntimeTypeEnv = ""
	if _, err := NewClaudeCodeAdapter(&fakeHost{}, spec); err == nil {
		t.Fatal("NewClaudeCodeAdapter must reject an empty launch env contract")
	}
}

func TestClaudeCodeSpawnStampsRuntimeAndCard(t *testing.T) {
	host := &fakeHost{}
	spec := testClaudeCodeSpec()
	a, err := NewClaudeCodeAdapter(host, spec)
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
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
	// The model pin: the standing process serves the runtime default model
	// from the first task when the envelope carries no override.
	if !hasEnv(host.specs[0].Env, claudeCodeModelEnv+"="+spec.DefaultModel.ID) {
		t.Fatalf("launch env %v missing %s=%s (default-model pin)", host.specs[0].Env, claudeCodeModelEnv, spec.DefaultModel.ID)
	}

	// The card must mirror the injected runtime spec's honest contract —
	// headless (no interactive approval) and the BYO endpoint advertised as
	// the gap it is.
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
	if card.Capabilities.InteractivePrompt {
		t.Fatal("claude-code card must not advertise interactive approval — headless -p")
	}
	if card.Capabilities.BYOModelEndpoint {
		t.Fatal("claude-code card must not advertise a BYO OpenAI-compatible endpoint — Anthropic wire")
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

// The envelope's model override replaces the standing process's default-model
// pin (the context window stays the runtime's authority).
func TestClaudeCodeModelOverridePinsModel(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewClaudeCodeAdapter(host, testClaudeCodeSpec())
	_, card, err := a.Spawn(context.Background(), Envelope{Model: "claude-opus-4"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !hasEnv(host.specs[0].Env, claudeCodeModelEnv+"=claude-opus-4") {
		t.Fatalf("launch env %v missing the override pin", host.specs[0].Env)
	}
	if card.Model.ID != "claude-opus-4" {
		t.Fatalf("card model = %q, want the override claude-opus-4", card.Model.ID)
	}
	if card.Model.ContextWindow != 200000 {
		t.Fatalf("card context window = %d, want the runtime's 200000 authority", card.Model.ContextWindow)
	}
}

// A BYO OpenAI-compatible ModelRoute must NOT be stamped onto the standing
// process: claude-code speaks the Anthropic wire, so the OPENAI_* lane other
// adapters stamp would be ignored — the card advertises the gap instead.
func TestClaudeCodeLaunchIgnoresOpenAICompatibleRoute(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewClaudeCodeAdapter(host, testClaudeCodeSpec())
	_, _, err := a.Spawn(context.Background(), Envelope{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "qwen3", Token: "rt"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	for _, unwanted := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "ANTHROPIC_BASE_URL"} {
		if hasEnv(host.specs[0].Env, unwanted+"=") || envKeyPresent(host.specs[0].Env, unwanted) {
			t.Fatalf("launch env %v stamps %s for an OpenAI-compatible route (want no mapping)", host.specs[0].Env, unwanted)
		}
	}
	// The route's model does not hijack the default-model pin either.
	if !hasEnv(host.specs[0].Env, claudeCodeModelEnv+"=claude-sonnet-4") {
		t.Fatalf("launch env %v missing the runtime default pin", host.specs[0].Env)
	}
}

// Caller-supplied env wins over the defaults: the resolved OAuth seat rides
// the envelope's env without the adapter interpreting it.
func TestClaudeCodeLaunchCallerEnvWins(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewClaudeCodeAdapter(host, testClaudeCodeSpec())
	_, _, err := a.Spawn(context.Background(), Envelope{
		Env: map[string]string{
			"CLAUDE_CODE_OAUTH_TOKEN": "seat-token-value",
			claudeCodeModelEnv:        "claude-haiku-4",
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !hasEnv(host.specs[0].Env, "CLAUDE_CODE_OAUTH_TOKEN=seat-token-value") {
		t.Fatalf("launch env %v missing the caller OAuth seat", host.specs[0].Env)
	}
	if !hasEnv(host.specs[0].Env, claudeCodeModelEnv+"=claude-haiku-4") {
		t.Fatalf("launch env %v: caller model pin must win", host.specs[0].Env)
	}
}

// The per-process source layout (ADR-0024 §1) overrides the plain WorkDir —
// the process runs in its own worktree.
func TestClaudeCodeLaunchSourceLayoutWins(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewClaudeCodeAdapter(host, testClaudeCodeSpec())
	_, _, err := a.Spawn(context.Background(), Envelope{
		WorkDir: "/plain",
		Source:  &SourceSpec{Branch: "b", WorktreeDir: "/wt/17"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if host.specs[0].WorkDir != "/wt/17" {
		t.Fatalf("launch workdir = %q, want the source worktree /wt/17", host.specs[0].WorkDir)
	}
}

func envKeyPresent(env []string, key string) bool {
	for _, e := range env {
		if len(e) > len(key) && e[:len(key)] == key && e[len(key)] == '=' {
			return true
		}
	}
	return false
}
