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

package capability

import (
	"encoding/json"
	"strings"
	"testing"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func scopedEndpoints() []Endpoint {
	return []Endpoint{
		{
			Name:                "github-mcp",
			Transport:           "streamable-http",
			URL:                 "https://mcp.example/github",
			AllowTools:          []string{"create_pull_request", "list_issues"},
			EnvNames:            []string{"KSQUAD_MCP_GITHUB_MCP_TOKEN"},
			CredentialSecretRef: &api.SecretRef{Name: "github-token"},
		},
	}
}

// TestRenderClaudeCodeScopesServerToAllowedTools is the plan §5 item-2
// acceptance shape: the runtime's MCP config contains github-mcp scoped
// to the allowed tools, credentials as env references — never literal.
func TestRenderClaudeCodeScopesServerToAllowedTools(t *testing.T) {
	raw, err := RenderClaudeCode(scopedEndpoints())
	require.NoError(t, err)

	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	entry, ok := doc.MCPServers["github-mcp"]
	require.True(t, ok, "github-mcp present")

	var server struct {
		Type              string            `json:"type"`
		URL               string            `json:"url"`
		Headers           map[string]string `json:"headers"`
		XKsquadAllowTools []string          `json:"x-ksquad-allow-tools"`
	}
	require.NoError(t, json.Unmarshal(entry, &server))
	assert.Equal(t, "http", server.Type)
	assert.Equal(t, "https://mcp.example/github", server.URL)
	assert.Equal(t, []string{"create_pull_request", "list_issues"}, server.XKsquadAllowTools)
	// Credential referenced, never literal.
	assert.Equal(t, "Bearer ${KSQUAD_MCP_GITHUB_MCP_TOKEN}", server.Headers["Authorization"])

	assert.NotContains(t, string(raw), "github-token\"}", "no secret name-as-value leakage")
}

func TestRenderClaudeCodeStdioEntry(t *testing.T) {
	raw, err := RenderClaudeCode([]Endpoint{stdioEndpoint()})
	require.NoError(t, err)
	var doc struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	ep := doc.MCPServers["gh-stdio"]
	assert.Equal(t, "stdio", ep.Type)
	assert.Equal(t, "gh-mcp-serve", ep.Command)
	assert.Equal(t, "${KSQUAD_MCP_GH_STDIO_TOKEN}", ep.Env["KSQUAD_MCP_GH_STDIO_TOKEN"])
}

func TestRenderOpenCodeNativeToolScope(t *testing.T) {
	raw, err := RenderOpenCode(scopedEndpoints())
	require.NoError(t, err)

	var doc struct {
		MCP map[string]struct {
			Type  string `json:"type"`
			URL   string `json:"url"`
			Tools *struct {
				Enable  []string `json:"enable"`
				Disable []string `json:"disable"`
			} `json:"tools"`
		} `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	ep := doc.MCP["github-mcp"]
	assert.Equal(t, "remote", ep.Type)
	require.NotNil(t, ep.Tools)
	assert.Equal(t, []string{"create_pull_request", "list_issues"}, ep.Tools.Enable)
}

func TestRenderOpenClawServersSection(t *testing.T) {
	raw, err := RenderOpenClaw(scopedEndpoints())
	require.NoError(t, err)

	var doc struct {
		MCP struct {
			Servers map[string]struct {
				Type         string   `json:"type"`
				URL          string   `json:"url"`
				AllowedTools []string `json:"allowedTools"`
			} `json:"servers"`
		} `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	ep := doc.MCP.Servers["github-mcp"]
	assert.Equal(t, "http", ep.Type)
	assert.Equal(t, []string{"create_pull_request", "list_issues"}, ep.AllowedTools)
}

// codexTOMLServer mirrors the codex config.toml [mcp_servers.<name>] shape for
// decode-side assertions.
type codexTOMLServer struct {
	Command           string            `toml:"command"`
	Args              []string          `toml:"args"`
	URL               string            `toml:"url"`
	BearerTokenEnvVar string            `toml:"bearer_token_env_var"`
	XKsquadAllowTools []string          `toml:"x-ksquad-allow-tools"`
	XKsquadDenyTools  []string          `toml:"x-ksquad-deny-tools"`
	Env               map[string]string `toml:"env"`
}

type codexTOMLProvider struct {
	BaseURL string `toml:"base_url"`
	WireAPI string `toml:"wire_api"`
}

type codexTOMLDoc struct {
	ModelProvider  string                       `toml:"model_provider"`
	MCPServers     map[string]codexTOMLServer   `toml:"mcp_servers"`
	ModelProviders map[string]codexTOMLProvider `toml:"model_providers"`
}

// TestRenderCodexEmitsValidTOMLWithByReferenceCreds is the AC1/AC3 shape: the
// HTTP endpoint renders to valid config.toml with url + a by-reference
// bearer_token_env_var (the env NAME, never the literal token), and the
// per-tool filter rides as the documented x-ksquad extension key.
func TestRenderCodexEmitsValidTOMLWithByReferenceCreds(t *testing.T) {
	raw, err := RenderCodex(scopedEndpoints())
	require.NoError(t, err)

	// AC3: the bytes parse as valid TOML.
	var doc codexTOMLDoc
	require.NoError(t, toml.Unmarshal(raw, &doc), "config.toml is valid TOML")

	srv, ok := doc.MCPServers["github-mcp"]
	require.True(t, ok, "github-mcp present")
	assert.Equal(t, "https://mcp.example/github", srv.URL)
	// AC1/AC3: credential is a by-reference env NAME, never the literal token.
	assert.Equal(t, "KSQUAD_MCP_GITHUB_MCP_TOKEN", srv.BearerTokenEnvVar)
	assert.Empty(t, srv.Command, "http endpoint has no command")
	// Per-tool filter → documented extension key (server-granularity, D3).
	assert.Equal(t, []string{"create_pull_request", "list_issues"}, srv.XKsquadAllowTools)

	// Defense in depth: the backing Secret name never leaks into the config.
	assert.NotContains(t, string(raw), "github-token", "no secret name leakage")
}

// TestRenderCodexStdioEntry covers the stdio transport: command/args plus an
// env sub-table whose values are ${VAR} name-references only (AC1/AC3).
func TestRenderCodexStdioEntry(t *testing.T) {
	raw, err := RenderCodex([]Endpoint{stdioEndpoint()})
	require.NoError(t, err)

	var doc codexTOMLDoc
	require.NoError(t, toml.Unmarshal(raw, &doc))
	srv := doc.MCPServers["gh-stdio"]
	assert.Equal(t, "gh-mcp-serve", srv.Command)
	assert.Equal(t, []string{"--org", "acme"}, srv.Args)
	// Credential by-reference: ${VAR}, never the literal token.
	assert.Equal(t, "${KSQUAD_MCP_GH_STDIO_TOKEN}", srv.Env["KSQUAD_MCP_GH_STDIO_TOKEN"])
	assert.NotContains(t, string(raw), "gh-token", "no secret name leakage")
}

// TestRenderCodexTaskIOServerAppears is the AC2 shape: the task-io Skill's MCP
// server (ADR-0004/ISI-3601/3602 — a stdio server the CLI reads relative to the
// workdir) appears in the rendered config.toml. config.toml itself lands in
// $CODEX_HOME=workdir (codex.go), so the workdir/task-io files contract holds.
func TestRenderCodexTaskIOServerAppears(t *testing.T) {
	taskIO := Endpoint{
		Name:       "task-io",
		Transport:  "stdio",
		Command:    "ksquad-task-io",
		Args:       []string{"serve", "--workdir", "."},
		AllowTools: []string{"read_task", "write_artifact"},
	}
	raw, err := RenderCodex([]Endpoint{taskIO})
	require.NoError(t, err)

	var doc codexTOMLDoc
	require.NoError(t, toml.Unmarshal(raw, &doc))
	srv, ok := doc.MCPServers["task-io"]
	require.True(t, ok, "task-io MCP server present")
	assert.Equal(t, "ksquad-task-io", srv.Command)
	assert.Equal(t, []string{"read_task", "write_artifact"}, srv.XKsquadAllowTools)
	// No credential: the task-io server rides no BYO Secret.
	assert.Empty(t, srv.Env)
	assert.Empty(t, srv.BearerTokenEnvVar)
}

// TestRenderCodexDeterministic guards stable manifest bytes across renders
// (map ordering is sorted by the encoder).
func TestRenderCodexDeterministic(t *testing.T) {
	eps := []Endpoint{stdioEndpoint(), scopedEndpoints()[0]}
	first, err := RenderCodex(eps)
	require.NoError(t, err)
	second, err := RenderCodex(eps)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
	// Both server tables are present.
	assert.True(t, strings.Contains(string(first), "[mcp_servers.gh-stdio]"))
	assert.True(t, strings.Contains(string(first), "[mcp_servers.github-mcp]"))
}

// TestRenderCodexBYOModelProvider is the S6 (FR6/AC9) shape: when a BYO model
// endpoint is set, RenderCodexConfig emits a [model_providers.ksquad-byo] block
// (base_url + wire_api) plus the top-level model_provider selector, alongside
// any MCP servers — and never the endpoint token (that rides OPENAI_API_KEY env).
func TestRenderCodexBYOModelProvider(t *testing.T) {
	raw, err := RenderCodexConfig([]Endpoint{stdioEndpoint()}, "http://ollama:11434/v1")
	require.NoError(t, err)

	var doc codexTOMLDoc
	require.NoError(t, toml.Unmarshal(raw, &doc), "config.toml is valid TOML")

	// Top-level selector points at the BYO provider.
	assert.Equal(t, "ksquad-byo", doc.ModelProvider)
	// The provider block carries the endpoint URL + OpenAI wire dialect.
	prov, ok := doc.ModelProviders["ksquad-byo"]
	require.True(t, ok, "model_providers.ksquad-byo present")
	assert.Equal(t, "http://ollama:11434/v1", prov.BaseURL)
	assert.Equal(t, "chat", prov.WireAPI)
	// MCP servers still render next to the provider block.
	_, ok = doc.MCPServers["gh-stdio"]
	assert.True(t, ok, "mcp servers preserved alongside model provider")
	// The endpoint token is never rendered into the persisted config.
	assert.NotContains(t, string(raw), "OPENAI_API_KEY", "token env not in config.toml")
	assert.NotContains(t, string(raw), "ollama-secret", "no token/secret leakage")
}

// TestRenderCodexNoModelProviderByDefault guards that the plain MCP-only render
// (no BYO endpoint) emits neither the selector nor a providers block, so the CLI
// keeps its built-in default provider.
func TestRenderCodexNoModelProviderByDefault(t *testing.T) {
	raw, err := RenderCodex([]Endpoint{stdioEndpoint()})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "model_provider", "no BYO provider unless endpoint set")

	var doc codexTOMLDoc
	require.NoError(t, toml.Unmarshal(raw, &doc))
	assert.Empty(t, doc.ModelProvider)
	assert.Empty(t, doc.ModelProviders)
}

func TestRenderHermesPassthroughIsIR(t *testing.T) {
	raw, err := RenderHermes(scopedEndpoints())
	require.NoError(t, err)

	var doc irDocument
	require.NoError(t, json.Unmarshal(raw, &doc))
	assert.Equal(t, 1, doc.Version)
	require.Len(t, doc.Endpoints, 1)
	assert.Equal(t, "github-mcp", doc.Endpoints[0].Name)
}

func TestRenderersRejectUnknownTransport(t *testing.T) {
	bad := []Endpoint{{Name: "x", Transport: "carrier-pigeon"}}
	for _, tc := range []struct {
		name string
		fn   func([]Endpoint) ([]byte, error)
	}{
		{"claude-code", RenderClaudeCode},
		{"opencode", RenderOpenCode},
		{"openclaw", RenderOpenClaw},
		{"codex", RenderCodex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.fn(bad)
			require.Error(t, err)
		})
	}
}
