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

package credential

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/credinject"
)

func agentWith(name string, spec api.AgentSpec) *api.Agent {
	return &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
}

// findEnv returns the SecretKeySelector-backed env var of the given name, or
// nil. It also fails the test if the env var is present but a literal Value
// (the whole point of the plumbing is that a credential is NEVER a literal).
func findEnv(t *testing.T, envs []corev1.EnvVar, name string) *corev1.EnvVar {
	t.Helper()
	for i := range envs {
		if envs[i].Name != name {
			continue
		}
		e := &envs[i]
		if e.Value != "" {
			t.Fatalf("env %q carries a literal value %q; credential/endpoint injection must be by-reference only", name, e.Value)
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("env %q is not a SecretKeyRef reference: %+v", name, e)
		}
		return e
	}
	return nil
}

// Story 7.2 — a Claude-family Agent with a human OAuth seat resolves to the
// CLAUDE_CODE_OAUTH_TOKEN env, by reference to the per-user Secret.
func TestResolve_ClaudeHumanSeat_7_2(t *testing.T) {
	agent := agentWith("alice-claude", api.AgentSpec{
		CredentialClass:     string(credinject.ClassHumanSeat),
		CredentialSecretRef: api.SecretRef{Name: "alice-claude-oauth"},
	})
	inj, err := Resolve(api.RuntimeTypeClaudeCode, agent)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := findEnv(t, inj.Env, "CLAUDE_CODE_OAUTH_TOKEN")
	if got == nil {
		t.Fatalf("missing CLAUDE_CODE_OAUTH_TOKEN; got env %+v", inj.Env)
	}
	if got.ValueFrom.SecretKeyRef.Name != "alice-claude-oauth" {
		t.Errorf("secret name = %q, want alice-claude-oauth", got.ValueFrom.SecretKeyRef.Name)
	}
}

// Story 7.3 — a second-runtime Agent (OpenClaw, service-account default class)
// resolves to a long-lived API-key env, by reference. Rotation = Secret update
// needs no code change: the env is a reference, so a rotated Secret is picked
// up on the next pod without touching this path.
func TestResolve_SecondRuntimeAPIKey_7_3(t *testing.T) {
	agent := agentWith("bob-openclaw", api.AgentSpec{
		// CredentialClass left empty → defaults to service-account.
		CredentialSecretRef: api.SecretRef{Name: "bob-openclaw-key"},
	})
	inj, err := Resolve(api.RuntimeTypeOpenClaw, agent)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := findEnv(t, inj.Env, "ANTHROPIC_API_KEY"); got == nil {
		t.Fatalf("missing ANTHROPIC_API_KEY; got env %+v", inj.Env)
	}
}

// Story 7.1 — the credential is per-namespace, never cross-squad: the injected
// env carries only a LocalObjectReference (a bare name), so the kubelet can
// only resolve it in the sandbox pod's own namespace. There is no field on
// which a cross-namespace Secret could be named.
func TestResolve_PerNamespaceLocalRef_7_1(t *testing.T) {
	agent := agentWith("carol", api.AgentSpec{
		CredentialSecretRef: api.SecretRef{Name: "carol-key"},
	})
	inj, err := Resolve(api.RuntimeTypeOpenCode, agent)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(inj.Env) == 0 {
		t.Fatalf("no credential env injected")
	}
	sel := inj.Env[0].ValueFrom.SecretKeyRef
	// LocalObjectReference has no Namespace field at all — assert the
	// reference is name-only, the structural guarantee behind 7.1.
	if sel.Name != "carol-key" {
		t.Errorf("secret ref name = %q, want carol-key", sel.Name)
	}
}

// Story 7.5 — a BYO Ollama endpoint (opencode) injects the endpoint URL into
// OPENAI_BASE_URL, by reference to the per-user endpoint Secret, alongside the
// provider credential env. Default endpoint key is honoured.
func TestResolve_BYOEndpoint_7_5(t *testing.T) {
	agent := agentWith("dave-ollama", api.AgentSpec{
		CredentialSecretRef: api.SecretRef{Name: "dave-key"},
		ModelEndpointRef:    &api.SecretRef{Name: "dave-ollama-endpoint"},
	})
	inj, err := Resolve(api.RuntimeTypeOpenCode, agent)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := findEnv(t, inj.Env, "OPENAI_BASE_URL")
	if got == nil {
		t.Fatalf("missing OPENAI_BASE_URL endpoint injection; got env %+v", inj.Env)
	}
	if got.ValueFrom.SecretKeyRef.Name != "dave-ollama-endpoint" {
		t.Errorf("endpoint secret = %q, want dave-ollama-endpoint", got.ValueFrom.SecretKeyRef.Name)
	}
	if got.ValueFrom.SecretKeyRef.Key != "endpointURL" {
		t.Errorf("endpoint key = %q, want default endpointURL", got.ValueFrom.SecretKeyRef.Key)
	}
	// The provider credential env must still be present next to the endpoint.
	if findEnv(t, inj.Env, "OPENAI_API_KEY") == nil {
		t.Errorf("provider credential env dropped when endpoint set; got %+v", inj.Env)
	}
}

// Story 7.5 — an explicit SecretRef.Key on the endpoint ref overrides the
// default endpointURL key.
func TestResolve_BYOEndpoint_KeyOverride_7_5(t *testing.T) {
	agent := agentWith("erin", api.AgentSpec{
		CredentialSecretRef: api.SecretRef{Name: "erin-key"},
		ModelEndpointRef:    &api.SecretRef{Name: "erin-endpoint", Key: "baseUrl"},
	})
	inj, err := Resolve(api.RuntimeTypeOpenCode, agent)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := findEnv(t, inj.Env, "OPENAI_BASE_URL")
	if got == nil || got.ValueFrom.SecretKeyRef.Key != "baseUrl" {
		t.Fatalf("endpoint key override not honoured; got %+v", got)
	}
}

// ISI-3647 S6 (FR6/AC9) — codex is OpenAI-compatible, so a BYO endpoint injects
// the endpoint URL into OPENAI_BASE_URL by reference (mirroring opencode); the
// endpoint token stays uninjected by the resolver (it rides the credential
// Secret / OPENAI_API_KEY, never a second env under the same header).
func TestResolve_BYOEndpoint_Codex_S6(t *testing.T) {
	agent := agentWith("frank-codex", api.AgentSpec{
		CredentialSecretRef: api.SecretRef{Name: "frank-key"},
		ModelEndpointRef:    &api.SecretRef{Name: "frank-endpoint"},
	})
	inj, err := Resolve(api.RuntimeTypeCodex, agent)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := findEnv(t, inj.Env, "OPENAI_BASE_URL")
	if got == nil {
		t.Fatalf("missing OPENAI_BASE_URL endpoint injection; got env %+v", inj.Env)
	}
	if got.ValueFrom.SecretKeyRef.Name != "frank-endpoint" {
		t.Errorf("endpoint secret = %q, want frank-endpoint", got.ValueFrom.SecretKeyRef.Name)
	}
	if got.ValueFrom.SecretKeyRef.Key != "endpointURL" {
		t.Errorf("endpoint key = %q, want default endpointURL", got.ValueFrom.SecretKeyRef.Key)
	}
	// The provider credential env is present; the endpoint token is NOT injected
	// separately by the resolver — no second by-value env under OPENAI_API_KEY.
	if findEnv(t, inj.Env, "OPENAI_API_KEY") == nil {
		t.Errorf("provider credential env dropped when endpoint set; got %+v", inj.Env)
	}
	for _, e := range inj.Env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Errorf("resolver emitted a by-value env %q; every value must be by-reference", e.Name)
		}
	}
}

// Fail-closed — a nil Agent, an empty credential ref, and an unmapped
// runtime/class pair each error rather than emit a silently-wrong pod.
func TestResolve_FailsClosed(t *testing.T) {
	if _, err := Resolve(api.RuntimeTypeClaudeCode, nil); err == nil {
		t.Error("nil Agent should error")
	}
	// Empty credential Secret name — credinject fails closed, we propagate.
	empty := agentWith("noref", api.AgentSpec{CredentialSecretRef: api.SecretRef{}})
	if _, err := Resolve(api.RuntimeTypeClaudeCode, empty); err == nil {
		t.Error("empty credential SecretRef should error")
	}
	// Human-seat OAuth on an API-key-only runtime (OpenClaw) — credinject has
	// no such mapping; must fail closed, not inject under the wrong name.
	badClass := agentWith("badclass", api.AgentSpec{
		CredentialClass:     string(credinject.ClassHumanSeat),
		CredentialSecretRef: api.SecretRef{Name: "x"},
	})
	if _, err := Resolve(api.RuntimeTypeOpenClaw, badClass); err == nil {
		t.Error("human-seat class on OpenClaw should fail closed")
	}
	// Unknown runtime with an endpoint ref — no base-URL mapping.
	unknownRT := agentWith("unknownrt", api.AgentSpec{
		CredentialSecretRef: api.SecretRef{Name: "x"},
		ModelEndpointRef:    &api.SecretRef{Name: "ep"},
	})
	if _, err := Resolve("no-such-runtime", unknownRT); err == nil {
		t.Error("unknown runtime should fail closed")
	}
}
