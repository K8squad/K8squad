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
	"github.com/K8squad/K8squad/pkg/capability"
)

// AC1 (ISI-3654): Given KSQUAD_RUNTIME_TYPE=codex, When runtimes.Get(codex),
// Then it resolves to the codex adapter keyed on the conformant RuntimeType
// constant (fail-closed if absent is covered by TestGetUnknownRuntimeFailsClosed).
func TestCodexResolves(t *testing.T) {
	rt, err := Get(apiv1alpha1.RuntimeTypeCodex)
	if err != nil {
		t.Fatalf("Get(codex) must resolve the registered adapter: %v", err)
	}
	if rt.Type() != "codex" {
		t.Errorf("codex.Type() = %q, want codex", rt.Type())
	}
	if rt.CLIVersion() != "rust-v0.152.0" {
		t.Errorf("codex.CLIVersion() = %q, want rust-v0.152.0 (pinned)", rt.CLIVersion())
	}
	if rt.CredentialShape() != ShapeAPIKey {
		t.Errorf("codex.CredentialShape() = %q, want api-key", rt.CredentialShape())
	}
	// D5: default model + context window are single-sourced.
	if dm := rt.DefaultModel(); dm.ID != "gpt-5.4-codex" || dm.ContextWindow != 272000 {
		t.Errorf("codex.DefaultModel() = %+v, want {gpt-5.4-codex 272000}", dm)
	}
}

// AC3 (ISI-3654): codex.Command() builds the pinned Codex ExecSpec — the
// ksquad-codex-exec wrapper (D4), the fixed flag set, credential mapped onto
// OPENAI_API_KEY + CODEX_HOME, and the envelope/credential kept out of argv.
func TestCodexCommandExecSpec(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeCodex)
	const secret = "sk-codex-secret-value"
	spec, err := rt.Command(LaunchContext{
		Envelope:   a2a.Envelope{SystemContext: "sys", Input: "do the thing"},
		Credential: secret,
		Model:      "gpt-5.4-codex",
		WorkDir:    "/work/run-1",
	})
	if err != nil {
		t.Fatalf("Command failed: %v", err)
	}
	if spec.Path != "ksquad-codex-exec" {
		t.Errorf("Path = %q, want ksquad-codex-exec (D4 stdin wrapper)", spec.Path)
	}
	if spec.WorkDir != "/work/run-1" {
		t.Errorf("WorkDir = %q, want /work/run-1", spec.WorkDir)
	}
	// Fixed flag set (order-sensitive for the paired flags).
	want := []string{"--json", "--skip-git-repo-check", "-m", "gpt-5.4-codex", "-C", "/work/run-1", "-s", "workspace-write", "-a", "never"}
	if strings.Join(spec.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", spec.Args, want)
	}
	// Credential maps onto OPENAI_API_KEY (Codex speaks the OpenAI wire).
	if !hasEnv(spec.Env, "OPENAI_API_KEY="+secret) {
		t.Errorf("expected OPENAI_API_KEY to carry the credential; env=%v", redact(spec.Env, secret))
	}
	if !hasEnv(spec.Env, "CODEX_HOME=/work/run-1") {
		t.Errorf("expected CODEX_HOME=<workdir>; env=%v", spec.Env)
	}
	// Envelope rides env, never argv (out of the process table).
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

// AC3: model precedence — ModelRoute.Model > Agent.spec.model > DefaultModel().ID.
func TestCodexModelPrecedence(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeCodex)
	cases := []struct {
		name string
		lc   LaunchContext
		want string
	}{
		{"route wins", LaunchContext{ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "route-model"}, Model: "agent-model"}, "route-model"},
		{"agent over default", LaunchContext{Model: "agent-model"}, "agent-model"},
		{"default when unset", LaunchContext{}, "gpt-5.4-codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := rt.Command(tc.lc)
			if err != nil {
				t.Fatal(err)
			}
			if !hasArg(spec.Args, tc.want) {
				t.Errorf("resolved model %q not in args %v", tc.want, spec.Args)
			}
		})
	}
}

// A BYO endpoint (story 5.7) rides OPENAI_BASE_URL + OPENAI_API_KEY and, because
// Codex maps both the credential and the route onto OPENAI_API_KEY, the route
// token must NOT be shadowed by a duplicate credential key.
func TestCodexBYOEndpointNoKeyCollision(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeCodex)
	spec, err := rt.Command(LaunchContext{
		Credential: "per-user-key",
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "llama3.1", Token: "route-token"},
		WorkDir:    "/work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasEnv(spec.Env, "OPENAI_BASE_URL=http://ollama:11434/v1") {
		t.Errorf("expected OPENAI_BASE_URL on the BYO wire; env=%v", spec.Env)
	}
	if !hasEnv(spec.Env, "OPENAI_API_KEY=route-token") {
		t.Errorf("expected the route token on OPENAI_API_KEY; env=%v", spec.Env)
	}
	if hasEnv(spec.Env, "OPENAI_API_KEY=per-user-key") {
		t.Errorf("per-user credential must not shadow the BYO route token; env=%v", spec.Env)
	}
}

// The MCP IR is rendered into config.toml only when the Run demands servers
// (RenderCodex is the S5 renderer, stubbed here). No servers => no file.
func TestCodexMCPWorkDirFile(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeCodex)

	empty, err := rt.Command(LaunchContext{WorkDir: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.WorkDirFiles) != 0 {
		t.Errorf("no MCP endpoints must materialize no config.toml; got %v", empty.WorkDirFiles)
	}

	withMCP, err := rt.Command(LaunchContext{
		WorkDir: "/work",
		MCPEndpoints: []capability.Endpoint{{
			Name:      "github",
			Transport: "stdio",
			Command:   "github-mcp",
			EnvNames:  []string{"GITHUB_TOKEN"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(withMCP.WorkDirFiles) != 1 || withMCP.WorkDirFiles[0].Name != "config.toml" {
		t.Fatalf("expected one config.toml workdir file; got %v", withMCP.WorkDirFiles)
	}
	if !strings.Contains(string(withMCP.WorkDirFiles[0].Content), "[mcp_servers.github]") {
		t.Errorf("rendered config.toml must carry the mcp_servers table; got %q", withMCP.WorkDirFiles[0].Content)
	}
}

// S6 (FR6/AC9): a BYO model endpoint materializes config.toml with the
// [model_providers.ksquad-byo] block — even with no MCP servers — so the CLI is
// pointed at the endpoint via config as well as OPENAI_BASE_URL (safe superset).
// The route token stays out of the persisted config.
func TestCodexBYOModelProviderConfig(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeCodex)
	spec, err := rt.Command(LaunchContext{
		WorkDir:    "/work",
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Token: "route-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.WorkDirFiles) != 1 || spec.WorkDirFiles[0].Name != "config.toml" {
		t.Fatalf("BYO endpoint alone must still materialize config.toml; got %v", spec.WorkDirFiles)
	}
	content := string(spec.WorkDirFiles[0].Content)
	if !strings.Contains(content, "[model_providers.ksquad-byo]") {
		t.Errorf("config.toml must carry the BYO model_providers block; got %q", content)
	}
	if !strings.Contains(content, `base_url = "http://ollama:11434/v1"`) {
		t.Errorf("model provider must point at the endpoint; got %q", content)
	}
	if strings.Contains(content, "route-token") {
		t.Errorf("route token must never land in the persisted config.toml; got %q", content)
	}
}
