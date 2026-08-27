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
	"testing"

	api "github.com/K8squad/K8squad/api/v1alpha1"

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
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.fn(bad)
			require.Error(t, err)
		})
	}
}
