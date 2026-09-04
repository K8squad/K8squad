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

package credinject

import (
	"strings"
	"testing"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// TestInject_RuntimeNativeMapping is the story 5.4 worked example: the same
// generic Secret maps to CLAUDE_CODE_OAUTH_TOKEN for a Claude-Code human seat
// and to an API-key env for a service-account credential.
func TestInject_RuntimeNativeMapping(t *testing.T) {
	cases := []struct {
		name    string
		runtime string
		class   CredentialClass
		ref     api.SecretRef
		wantEnv string
		wantKey string
	}{
		{
			name:    "claude-code human seat -> OAuth token env",
			runtime: api.RuntimeTypeClaudeCode,
			class:   ClassHumanSeat,
			ref:     api.SecretRef{Name: "alice-claude"},
			wantEnv: "CLAUDE_CODE_OAUTH_TOKEN",
			wantKey: "token",
		},
		{
			name:    "claude-code service account -> API key env",
			runtime: api.RuntimeTypeClaudeCode,
			class:   ClassServiceAccount,
			ref:     api.SecretRef{Name: "alice-anthropic"},
			wantEnv: "ANTHROPIC_API_KEY",
			wantKey: "apiKey",
		},
		{
			name:    "openclaw service account -> API key env",
			runtime: api.RuntimeTypeOpenClaw,
			class:   ClassServiceAccount,
			ref:     api.SecretRef{Name: "bob-key"},
			wantEnv: "ANTHROPIC_API_KEY",
			wantKey: "apiKey",
		},
		{
			name:    "opencode service account -> OpenAI-compatible env",
			runtime: api.RuntimeTypeOpenCode,
			class:   ClassServiceAccount,
			ref:     api.SecretRef{Name: "carol-openai"},
			wantEnv: "OPENAI_API_KEY",
			wantKey: "apiKey",
		},
		{
			// ISI-3647 S4 (FR4/AC1): a BYO OpenAI key for a codex Agent rides
			// the OpenAI-standard env, injected by reference. "codex" is the
			// literal the RuntimeTypeCodex const (S2) will hold.
			name:    "codex service account -> OPENAI_API_KEY env",
			runtime: "codex",
			class:   ClassServiceAccount,
			ref:     api.SecretRef{Name: "dave-openai"},
			wantEnv: "OPENAI_API_KEY",
			wantKey: "apiKey",
		},
		{
			name:    "explicit Secret key overrides the default",
			runtime: api.RuntimeTypeClaudeCode,
			class:   ClassHumanSeat,
			ref:     api.SecretRef{Name: "alice-claude", Key: "oauth"},
			wantEnv: "CLAUDE_CODE_OAUTH_TOKEN",
			wantKey: "oauth",
		},
		{
			name:    "empty class resolves to the service-account default",
			runtime: api.RuntimeTypeClaudeCode,
			class:   "",
			ref:     api.SecretRef{Name: "alice-anthropic"},
			wantEnv: "ANTHROPIC_API_KEY",
			wantKey: "apiKey",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inj, err := Inject(tc.runtime, tc.class, tc.ref)
			if err != nil {
				t.Fatalf("Inject: unexpected error: %v", err)
			}
			if len(inj.Env) != 1 {
				t.Fatalf("Inject: want 1 env var, got %d", len(inj.Env))
			}
			got := inj.Env[0]
			if got.Name != tc.wantEnv {
				t.Errorf("env name: want %q, got %q", tc.wantEnv, got.Name)
			}
			if got.ValueFrom == nil || got.ValueFrom.SecretKeyRef == nil {
				t.Fatalf("env %q must inject by reference (ValueFrom.SecretKeyRef), got %+v", got.Name, got)
			}
			if got.ValueFrom.SecretKeyRef.Name != tc.ref.Name {
				t.Errorf("secret name: want %q, got %q", tc.ref.Name, got.ValueFrom.SecretKeyRef.Name)
			}
			if got.ValueFrom.SecretKeyRef.Key != tc.wantKey {
				t.Errorf("secret key: want %q, got %q", tc.wantKey, got.ValueFrom.SecretKeyRef.Key)
			}
		})
	}
}

// TestInject_NeverEmbedsLiteralValue is the NFR-SEC3 structural guarantee: the
// contract injects the credential BY REFERENCE and never sets a literal Value,
// so the control plane never handles (and so can never log/persist) the
// plaintext.
func TestInject_NeverEmbedsLiteralValue(t *testing.T) {
	inj, err := Inject(api.RuntimeTypeClaudeCode, ClassHumanSeat, api.SecretRef{Name: "alice-claude"})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for _, e := range inj.Env {
		if e.Value != "" {
			t.Errorf("env %q carries a literal Value %q; credential must ride ValueFrom.SecretKeyRef only", e.Name, e.Value)
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Errorf("env %q must be injected by SecretKeyRef reference", e.Name)
		}
	}
}

// TestInject_UnknownRuntimeFailsClosed proves an unmapped runtime is rejected
// rather than silently defaulted — a wrong env name would authenticate the Run
// as nobody.
func TestInject_UnknownRuntimeFailsClosed(t *testing.T) {
	_, err := Inject("vendor-mystery-shim", ClassServiceAccount, api.SecretRef{Name: "x"})
	if err == nil {
		t.Fatal("Inject: want fail-closed error for unknown runtime, got nil")
	}
	if !strings.Contains(err.Error(), "vendor-mystery-shim") {
		t.Errorf("error should name the offending runtime, got %q", err.Error())
	}
}

// TestInject_ClassNotSupportedByRuntimeFailsClosed proves a human-seat OAuth
// class on an API-key-only runtime (OpenClaw) is rejected — the mismatch cannot
// silently pick the service-account row.
func TestInject_ClassNotSupportedByRuntimeFailsClosed(t *testing.T) {
	_, err := Inject(api.RuntimeTypeOpenClaw, ClassHumanSeat, api.SecretRef{Name: "x"})
	if err == nil {
		t.Fatal("Inject: want fail-closed error for unsupported (runtime, class) pair, got nil")
	}
	if !strings.Contains(err.Error(), string(ClassHumanSeat)) {
		t.Errorf("error should name the unsupported class, got %q", err.Error())
	}
}

// TestInject_CodexHumanSeatFailsClosed proves the ToS-gated human-seat auth.json
// branch (ISI-3647 S9) is deliberately unmapped for codex in v1: a human-seat
// class on codex fails CLOSED rather than falling back to the service-account
// row, so no Run authenticates under an env the codex CLI ignores.
func TestInject_CodexHumanSeatFailsClosed(t *testing.T) {
	_, err := Inject("codex", ClassHumanSeat, api.SecretRef{Name: "x"})
	if err == nil {
		t.Fatal("Inject: want fail-closed error for (codex, human-seat) unmapped pair, got nil")
	}
	if !strings.Contains(err.Error(), string(ClassHumanSeat)) {
		t.Errorf("error should name the unsupported class, got %q", err.Error())
	}
}

// TestInject_EmptySecretNameFailsClosed guards the dangling-ref case at the
// injection layer (the webhook also existence-checks the Secret).
func TestInject_EmptySecretNameFailsClosed(t *testing.T) {
	if _, err := Inject(api.RuntimeTypeClaudeCode, ClassServiceAccount, api.SecretRef{}); err == nil {
		t.Fatal("Inject: want error for empty Secret name, got nil")
	}
}

func TestValidateClass(t *testing.T) {
	valid := []CredentialClass{"", ClassHumanSeat, ClassServiceAccount}
	for _, c := range valid {
		if err := ValidateClass(c); err != nil {
			t.Errorf("ValidateClass(%q): unexpected error %v", c, err)
		}
	}
	if err := ValidateClass("root"); err == nil {
		t.Error("ValidateClass(\"root\"): want error, got nil")
	}
}

func TestResolveDefaultsToServiceAccount(t *testing.T) {
	if got := Resolve(""); got != ClassServiceAccount {
		t.Errorf("Resolve(\"\"): want %q, got %q", ClassServiceAccount, got)
	}
	if got := Resolve(ClassHumanSeat); got != ClassHumanSeat {
		t.Errorf("Resolve(human-seat): want passthrough, got %q", got)
	}
}

// TestKnownClassesStable keeps the webhook enum and the taxonomy in lockstep:
// if a class is added, this list (the webhook's source of truth) must grow too.
func TestKnownClassesStable(t *testing.T) {
	got := KnownClasses()
	if len(got) != 2 {
		t.Fatalf("KnownClasses: want 2, got %d (%v)", len(got), got)
	}
}

// TestDefaultSecretKeyIsTheReadKey — the exported read-key seam (AD-6/F1): the
// key a writer derives MUST equal the key Inject projects when SecretRef.Key
// is empty, for every mapped (runtime, class) pair, and unmapped pairs fail
// closed. Pinning this against Inject (not a literal) is what keeps write/read
// agreement from drifting.
func TestDefaultSecretKeyIsTheReadKey(t *testing.T) {
	for rt, byClass := range table {
		for class, b := range byClass {
			got, ok := DefaultSecretKey(rt, class)
			if !ok {
				t.Errorf("DefaultSecretKey(%q, %q): want ok, the pair is mapped", rt, class)
				continue
			}
			if got != b.defaultKey {
				t.Errorf("DefaultSecretKey(%q, %q): want %q, got %q", rt, class, b.defaultKey, got)
			}
			inj, err := Inject(rt, class, api.SecretRef{Name: "cred"})
			if err != nil {
				t.Errorf("Inject(%q, %q): %v", rt, class, err)
				continue
			}
			if inj.Env[0].ValueFrom.SecretKeyRef.Key != got {
				t.Errorf("Inject(%q, %q) reads key %q but DefaultSecretKey says %q", rt, class,
					inj.Env[0].ValueFrom.SecretKeyRef.Key, got)
			}
		}
	}
	if _, ok := DefaultSecretKey("nope", ClassServiceAccount); ok {
		t.Error("unknown runtime must fail closed")
	}
	if _, ok := DefaultSecretKey(api.RuntimeTypeOpenClaw, ClassHumanSeat); ok {
		t.Error("unmapped (runtime, class) pair must fail closed")
	}
	// Empty class resolves to the default, exactly like Inject.
	if got, ok := DefaultSecretKey(api.RuntimeTypeClaudeCode, ""); !ok || got != "apiKey" {
		t.Errorf("empty class ⇒ default key: want apiKey/true, got %q/%v", got, ok)
	}
}
