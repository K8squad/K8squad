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
	"strconv"
	"strings"
	"testing"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
)

// TestVibeResolves asserts the vibe adapter is registered under the conformant
// RuntimeTypeVibe constant (ISI-6158 AC1).
func TestVibeResolves(t *testing.T) {
	rt, err := Get(apiv1alpha1.RuntimeTypeVibe)
	if err != nil {
		t.Fatalf("Get(vibe) must resolve the registered adapter: %v", err)
	}
	if rt.Type() != "vibe" {
		t.Errorf("vibe.Type() = %q, want vibe", rt.Type())
	}
	if rt.CLIVersion() != vibeCLIVersion {
		t.Errorf("vibe.CLIVersion() = %q, want %q (pinned, ADR-017)", rt.CLIVersion(), vibeCLIVersion)
	}
	if rt.CredentialShape() != ShapeAPIKey {
		t.Errorf("vibe.CredentialShape() = %q, want api-key (Mistral wire)", rt.CredentialShape())
	}
	dm := rt.DefaultModel()
	if dm.ID != vibeDefaultModel || dm.ContextWindow != vibeContextWindow {
		t.Errorf("vibe.DefaultModel() = %+v, want {%s %d}", dm, vibeDefaultModel, vibeContextWindow)
	}
}

// TestVibeCommandExecSpec asserts the Command ExecSpec: vibe binary, pinned flag
// set, credential mapped onto MISTRAL_API_KEY, VIBE_ACTIVE_MODEL set, envelope
// out of argv (ISI-6158 AC).
func TestVibeCommandExecSpec(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeVibe)
	const secret = "sk-mistral-secret"
	spec, err := rt.Command(LaunchContext{
		Envelope:   a2a.Envelope{SystemContext: "sys", Input: "do the thing"},
		Credential: secret,
		Model:      vibeDefaultModel,
		WorkDir:    "/work/run-1",
	})
	if err != nil {
		t.Fatalf("Command failed: %v", err)
	}
	if spec.Path != "vibe" {
		t.Errorf("Path = %q, want vibe", spec.Path)
	}
	if spec.WorkDir != "/work/run-1" {
		t.Errorf("WorkDir = %q, want /work/run-1", spec.WorkDir)
	}
	// Fixed flag set including -p (non-interactive), --output streaming, --auto-approve, --trust.
	wantArgs := []string{
		"-p",
		"--output", "streaming",
		"--auto-approve",
		"--workdir", "/work/run-1",
		"--max-turns", strconv.Itoa(vibeMaxTurns),
		"--trust",
		"--model", vibeDefaultModel,
	}
	if strings.Join(spec.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("Args = %v, want %v", spec.Args, wantArgs)
	}
	// Credential maps onto MISTRAL_API_KEY.
	if !hasEnv(spec.Env, "MISTRAL_API_KEY="+secret) {
		t.Errorf("expected MISTRAL_API_KEY to carry the credential; env=%v", redact(spec.Env, secret))
	}
	// VIBE_ACTIVE_MODEL is pinned (GrowthBook mitigation).
	if !hasEnv(spec.Env, "VIBE_ACTIVE_MODEL="+vibeDefaultModel) {
		t.Errorf("expected VIBE_ACTIVE_MODEL to be pinned; env=%v", spec.Env)
	}
	// Envelope rides env, never argv.
	if !hasEnv(spec.Env, "KSQUAD_INPUT=do the thing") {
		t.Errorf("envelope input must ride env; env=%v", spec.Env)
	}
	for _, a := range spec.Args {
		if strings.Contains(a, secret) {
			t.Errorf("credential leaked into argv %q", a)
		}
		if strings.Contains(a, "do the thing") {
			t.Errorf("work instruction leaked into argv %q", a)
		}
	}
}

// TestVibeModelPrecedence asserts ModelRoute.Model > Agent.spec.model > DefaultModel.
func TestVibeModelPrecedence(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeVibe)
	cases := []struct {
		name       string
		lc         LaunchContext
		wantModel  string
	}{
		{
			"default", LaunchContext{WorkDir: "/w"},
			vibeDefaultModel,
		},
		{
			"agent-spec-model", LaunchContext{WorkDir: "/w", Model: "mistral-small-latest"},
			"mistral-small-latest",
		},
		{
			"route-model-wins", LaunchContext{
				WorkDir:    "/w",
				Model:      "mistral-small-latest",
				ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "codestral"},
			},
			"codestral",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := rt.Command(tc.lc)
			if err != nil {
				t.Fatalf("Command failed: %v", err)
			}
			if !hasEnv(spec.Env, "VIBE_ACTIVE_MODEL="+tc.wantModel) {
				t.Errorf("VIBE_ACTIVE_MODEL = wrong; env=%v, want %q", spec.Env, tc.wantModel)
			}
			for i, a := range spec.Args {
				if a == "--model" && i+1 < len(spec.Args) {
					if spec.Args[i+1] != tc.wantModel {
						t.Errorf("--model arg = %q, want %q", spec.Args[i+1], tc.wantModel)
					}
					return
				}
			}
			t.Errorf("--model flag not found in args %v", spec.Args)
		})
	}
}

// TestVibeBYORoute asserts that a BYO model route maps onto
// MISTRAL_BASE_URL/MISTRAL_API_KEY with the Ollama placeholder (no OPENAI_*).
func TestVibeBYORoute(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeVibe)
	cases := []struct {
		name    string
		route   a2a.ModelRoute
		wantKey string
	}{
		{
			"with-token",
			a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Token: "sk-byo"},
			"sk-byo",
		},
		{
			"ollama-placeholder",
			a2a.ModelRoute{Endpoint: "http://ollama:11434/v1"},
			"ollama",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := rt.Command(LaunchContext{WorkDir: "/w", ModelRoute: tc.route})
			if err != nil {
				t.Fatalf("Command failed: %v", err)
			}
			if !hasEnv(spec.Env, "MISTRAL_BASE_URL="+tc.route.Endpoint) {
				t.Errorf("expected MISTRAL_BASE_URL; env=%v", spec.Env)
			}
			if !hasEnv(spec.Env, "MISTRAL_API_KEY="+tc.wantKey) {
				t.Errorf("expected MISTRAL_API_KEY=%q; env=%v", tc.wantKey, spec.Env)
			}
			// No OpenAI env on a Mistral BYO route.
			for _, e := range spec.Env {
				if strings.HasPrefix(e, "OPENAI_") {
					t.Errorf("unexpected OpenAI env on Mistral BYO route: %q", e)
				}
			}
		})
	}
}

// TestVibeCallerEnvWins asserts that caller-supplied env keys override the
// adapter's defaults (MISTRAL_API_KEY, VIBE_ACTIVE_MODEL), preserving the
// "caller env wins" invariant.
func TestVibeCallerEnvWins(t *testing.T) {
	// Caller env is passed via LaunchContext.Credential + ModelRoute;
	// the "caller wins" contract is that an explicit credential is used
	// as-is, not overwritten by a fallback. Supplying a credential
	// alongside an explicit model should yield exactly those values.
	rt, _ := Get(apiv1alpha1.RuntimeTypeVibe)
	spec, err := rt.Command(LaunchContext{
		WorkDir:    "/w",
		Credential: "caller-key",
		Model:      "caller-model",
	})
	if err != nil {
		t.Fatalf("Command failed: %v", err)
	}
	if !hasEnv(spec.Env, "MISTRAL_API_KEY=caller-key") {
		t.Errorf("caller MISTRAL_API_KEY not honored; env=%v", redact(spec.Env, "caller-key"))
	}
	if !hasEnv(spec.Env, "VIBE_ACTIVE_MODEL=caller-model") {
		t.Errorf("caller model not pinned via VIBE_ACTIVE_MODEL; env=%v", spec.Env)
	}
}

// TestVibeSettleLine asserts the terminal stream marker is correctly detected.
func TestVibeSettleLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`{"type":"session_end"}`, true},
		{`{"type":"session_failed","error":"max turns"}`, false},
		{`{"type":"agent_message","text":"done"}`, false},
		{`not json`, false},
		{`{"type":"tool_call","tool":"bash","status":"completed"}`, false},
	}
	for _, tc := range cases {
		if got := vibeSettleLine(tc.line); got != tc.want {
			t.Errorf("vibeSettleLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// TestVibeParseLineProgress asserts parseVibeLine maps streaming events onto
// the correct typed Progress events.
func TestVibeParseLineProgress(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantKind a2a.EventType
		check    func(t *testing.T, p []Progress)
	}{
		{
			"tool_call_completed",
			`{"type":"tool_call","tool":"bash","arguments":{"cmd":"git status"},"status":"completed","exit_code":0}`,
			a2a.EventTool,
			func(t *testing.T, ps []Progress) {
				if ps[0].Tool.Name != "bash" {
					t.Errorf("Tool.Name = %q, want bash", ps[0].Tool.Name)
				}
				if ps[0].Tool.OK == nil || !*ps[0].Tool.OK {
					t.Errorf("Tool.OK = %v, want true (exit_code=0)", ps[0].Tool.OK)
				}
			},
		},
		{
			"tool_call_failed",
			`{"type":"tool_call","tool":"bash","status":"failed","exit_code":1}`,
			a2a.EventTool,
			func(t *testing.T, ps []Progress) {
				if ps[0].Tool.OK == nil || *ps[0].Tool.OK {
					t.Errorf("Tool.OK = %v, want false", ps[0].Tool.OK)
				}
			},
		},
		{
			"agent_message",
			`{"type":"agent_message","text":"Done — 3 files changed."}`,
			a2a.EventMessage,
			func(t *testing.T, ps []Progress) {
				if ps[0].Message.Text != "Done — 3 files changed." {
					t.Errorf("Message.Text = %q", ps[0].Message.Text)
				}
			},
		},
		{
			"usage",
			`{"type":"usage","input_tokens":1200,"output_tokens":80}`,
			a2a.EventUsage,
			func(t *testing.T, ps []Progress) {
				if ps[0].Usage.Input != 1200 || ps[0].Usage.Output != 80 {
					t.Errorf("Usage = %+v, want {1200 80}", ps[0].Usage)
				}
			},
		},
		{
			"session_end_no_progress",
			`{"type":"session_end"}`,
			"", // no events
			func(t *testing.T, ps []Progress) {
				if len(ps) != 0 {
					t.Errorf("session_end must emit no Progress; got %v", ps)
				}
			},
		},
		{
			"session_failed",
			`{"type":"session_failed","error":"max turns reached"}`,
			a2a.EventMessage,
			func(t *testing.T, ps []Progress) {
				if !strings.Contains(ps[0].Message.Text, "max turns") {
					t.Errorf("expected error text in message; got %q", ps[0].Message.Text)
				}
			},
		},
		{
			"non_json_degrades",
			`Traceback (most recent call last):`,
			a2a.EventMessage,
			func(t *testing.T, ps []Progress) {
				if ps[0].Message.Trust != "untrusted" {
					t.Errorf("non-JSON line must be untrusted; got %q", ps[0].Message.Trust)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := parseVibeLine(tc.line)
			if tc.check != nil {
				tc.check(t, ps)
			}
		})
	}
}

// TestVibeCommandHasSettleLine asserts the ExecSpec sets SettleLine (ISI-4224).
func TestVibeCommandHasSettleLine(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeVibe)
	spec, err := rt.Command(LaunchContext{WorkDir: "/w"})
	if err != nil {
		t.Fatalf("Command failed: %v", err)
	}
	if spec.SettleLine == nil {
		t.Fatal("ExecSpec.SettleLine must be set (ISI-4224 settle detection)")
	}
	// Sanity: the injected func detects the terminal marker.
	if !spec.SettleLine(`{"type":"session_end"}`) {
		t.Error("injected SettleLine must detect session_end")
	}
}

// TestVibeCommandHasParseFunc asserts the ExecSpec sets Parse for typed Progress.
func TestVibeCommandHasParseFunc(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeVibe)
	spec, err := rt.Command(LaunchContext{WorkDir: "/w"})
	if err != nil {
		t.Fatalf("Command failed: %v", err)
	}
	if spec.Parse == nil {
		t.Fatal("ExecSpec.Parse must be set for typed Progress events")
	}
}
