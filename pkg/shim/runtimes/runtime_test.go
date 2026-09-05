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

// TestV1ShimSetRegistered asserts the v1 shim set (stories 5.5 + 5.8) is
// registered and keyed on the conformant RuntimeType constants (FR-D3).
func TestV1ShimSetRegistered(t *testing.T) {
	want := []string{
		apiv1alpha1.RuntimeTypeCodex,
		apiv1alpha1.RuntimeTypeHermes,
		apiv1alpha1.RuntimeTypeOpenClaw,
		apiv1alpha1.RuntimeTypeOpenCode,
	}
	got := Registered()
	if len(got) != len(want) {
		t.Fatalf("Registered() = %v, want the v1 set %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("Registered()[%d] = %q, want %q", i, got[i], w)
		}
	}

	for _, typ := range want {
		rt, err := Get(typ)
		if err != nil {
			t.Fatalf("Get(%q) failed: %v", typ, err)
		}
		if rt.Type() != typ {
			t.Errorf("Get(%q).Type() = %q", typ, rt.Type())
		}
	}
}

func TestGetUnknownRuntimeFailsClosed(t *testing.T) {
	if _, err := Get("claude-code"); err == nil {
		t.Fatal("Get(claude-code) should fail closed (Phase 2, not in v1 set)")
	}
}

// TestCapabilityHonesty asserts every v1 runtime declares streaming (SSE V2 is
// mandatory) and that the deliberate capability gaps are advertised as
// first-class flags rather than hidden (story 5.2, F15).
func TestCapabilityHonesty(t *testing.T) {
	for _, typ := range Registered() {
		rt, _ := Get(typ)
		caps := rt.Capabilities()
		if !caps.Streaming {
			t.Errorf("%s: streaming must be true (SSE V2 mandatory)", typ)
		}
		if caps.ArtifactKinds == nil {
			t.Errorf("%s: artifactKinds must be declared, not nil", typ)
		}
	}

	// The specific advertised gaps that make capability honesty testable.
	hermesRT, _ := Get(apiv1alpha1.RuntimeTypeHermes)
	if hermesRT.Capabilities().InteractivePrompt {
		t.Error("hermes must advertise interactivePrompt=false (no input-required turn)")
	}
	ocRT, _ := Get(apiv1alpha1.RuntimeTypeOpenCode)
	if ocRT.Capabilities().PackageInstall {
		t.Error("opencode must advertise packageInstall=false (owns its own sandbox)")
	}
}

// TestCredentialMapping asserts each runtime maps the generic Secret into its
// native env var (story 5.4) and never emits the raw value into argv (where it
// would leak through the process table).
func TestCredentialMapping(t *testing.T) {
	const secret = "super-secret-token-value"
	cases := map[string]string{
		apiv1alpha1.RuntimeTypeOpenClaw: "OPENCLAW_API_KEY",
		apiv1alpha1.RuntimeTypeHermes:   "HERMES_API_KEY",
		apiv1alpha1.RuntimeTypeOpenCode: "OPENCODE_API_KEY",
		// Codex speaks the OpenAI wire natively, so the per-user credential maps
		// onto OPENAI_API_KEY (ShapeAPIKey, D1; epic ISI-3647, gate ISI-3660).
		apiv1alpha1.RuntimeTypeCodex: "OPENAI_API_KEY",
	}
	for typ, wantVar := range cases {
		rt, _ := Get(typ)
		spec, err := rt.Command(LaunchContext{
			Envelope:   a2a.Envelope{Input: "do the thing"},
			Credential: secret,
			Model:      "some-model",
		})
		if err != nil {
			t.Fatalf("%s: Command failed: %v", typ, err)
		}
		if !hasEnv(spec.Env, wantVar+"="+secret) {
			t.Errorf("%s: expected env %s to carry the credential; env=%v", typ, wantVar, redact(spec.Env, secret))
		}
		for _, a := range spec.Args {
			if strings.Contains(a, secret) {
				t.Errorf("%s: credential leaked into argv %q", typ, a)
			}
		}
		// The prompt must ride env, never argv.
		for _, a := range spec.Args {
			if strings.Contains(a, "do the thing") {
				t.Errorf("%s: work instruction leaked into argv %q", typ, a)
			}
		}
	}
}

// TestBYOModelEndpointWire asserts the resolved BYO endpoint (story 5.7) is
// mapped onto the OpenAI-compatible env and that the empty-token Ollama lane
// (story 5.6) gets the conventional placeholder key.
func TestBYOModelEndpointWire(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeOpenClaw)
	spec, err := rt.Command(LaunchContext{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "llama3.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasEnv(spec.Env, "OPENAI_BASE_URL=http://ollama:11434/v1") {
		t.Errorf("expected OPENAI_BASE_URL on the endpoint wire; env=%v", spec.Env)
	}
	if !hasEnv(spec.Env, "OPENAI_API_KEY=ollama") {
		t.Errorf("expected empty-token Ollama lane to use the 'ollama' placeholder key; env=%v", spec.Env)
	}
	// The route's model wins over the runtime default.
	if !hasArg(spec.Args, "llama3.1") {
		t.Errorf("expected route model llama3.1 in args; got %v", spec.Args)
	}
}

// TestCodexBYOModelEndpointWire pins the codex BYO endpoint wire (story 5.7/D6,
// epic ISI-3647): a resolved model route maps onto the OpenAI-compatible env,
// the route's own token becomes OPENAI_API_KEY, the route model wins over the
// runtime default, and a config.toml is rendered so the CLI's
// [model_providers.*] block points at the endpoint. Critically, a per-user
// credential present alongside a route must NOT also emit OPENAI_API_KEY —
// glibc getenv is first-wins, so a second key would shadow the route token.
func TestCodexBYOModelEndpointWire(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeCodex)
	spec, err := rt.Command(LaunchContext{
		Envelope:   a2a.Envelope{Input: "do the thing"},
		Credential: "per-user-key-should-be-shadowed",
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "qwen3", Token: "route-token"},
		WorkDir:    "/tmp/codex-work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasEnv(spec.Env, "OPENAI_BASE_URL=http://ollama:11434/v1") {
		t.Errorf("expected OPENAI_BASE_URL on the endpoint wire; env=%v", spec.Env)
	}
	if !hasEnv(spec.Env, "OPENAI_API_KEY=route-token") {
		t.Errorf("expected the route token to become OPENAI_API_KEY; env=%v", spec.Env)
	}
	// Exactly one OPENAI_API_KEY — the per-user credential must not shadow the route.
	if n := countEnvKey(spec.Env, "OPENAI_API_KEY"); n != 1 {
		t.Errorf("expected exactly one OPENAI_API_KEY (route wins, no shadow), got %d; env=%v", n, spec.Env)
	}
	if hasEnv(spec.Env, "OPENAI_API_KEY=per-user-key-should-be-shadowed") {
		t.Error("per-user credential leaked as OPENAI_API_KEY alongside the route token (would shadow it)")
	}
	// The route's model wins over the runtime default.
	if !hasArg(spec.Args, "qwen3") {
		t.Errorf("expected route model qwen3 in args; got %v", spec.Args)
	}
	// A config.toml is rendered so the CLI routes to the BYO endpoint (D6).
	var gotConfig bool
	for _, f := range spec.WorkDirFiles {
		if f.Name == "config.toml" {
			gotConfig = true
		}
	}
	if !gotConfig {
		t.Errorf("expected a rendered config.toml for the BYO endpoint; files=%v", spec.WorkDirFiles)
	}
	// The prompt rides env, never argv (no leak into the process table).
	for _, a := range spec.Args {
		if strings.Contains(a, "do the thing") {
			t.Errorf("work instruction leaked into argv %q", a)
		}
	}
}

func countEnvKey(env []string, key string) int {
	n := 0
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			n++
		}
	}
	return n
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func hasArg(args []string, substr string) bool {
	for _, a := range args {
		if strings.Contains(a, substr) {
			return true
		}
	}
	return false
}

func redact(env []string, secret string) []string {
	out := make([]string, len(env))
	for i, e := range env {
		out[i] = strings.ReplaceAll(e, secret, "***")
	}
	return out
}
