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
