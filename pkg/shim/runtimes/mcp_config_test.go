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
	"encoding/json"
	"testing"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mcpEndpoints() []capability.Endpoint {
	return []capability.Endpoint{
		{
			Name:                "github-mcp",
			Transport:           "streamable-http",
			URL:                 "https://mcp.example/github",
			AllowTools:          []string{"create_pull_request"},
			EnvNames:            []string{"KSQUAD_MCP_GITHUB_MCP_TOKEN"},
			CredentialSecretRef: &apiv1alpha1.SecretRef{Name: "github-token"},
		},
	}
}

func TestOpenCodeRendersMCPConfigToWorkDir(t *testing.T) {
	spec, err := Get("opencode")
	require.NoError(t, err)
	exec, err := spec.Command(LaunchContext{MCPEndpoints: mcpEndpoints(), WorkDir: "/w"})
	require.NoError(t, err)
	require.Len(t, exec.WorkDirFiles, 1)
	f := exec.WorkDirFiles[0]
	assert.Equal(t, "opencode.json", f.Name)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(f.Content, &doc))
	mcp, ok := doc["mcp"].(map[string]any)
	require.True(t, ok)
	server, ok := mcp["github-mcp"].(map[string]any)
	require.True(t, ok)
	tools, ok := server["tools"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"create_pull_request"}, tools["enable"])
	assert.NotContains(t, string(f.Content), "github-token", "no secret material")
}

func TestOpenClawRendersMCPConfigToWorkDir(t *testing.T) {
	spec, err := Get("openclaw")
	require.NoError(t, err)
	exec, err := spec.Command(LaunchContext{MCPEndpoints: mcpEndpoints(), WorkDir: "/w"})
	require.NoError(t, err)
	require.Len(t, exec.WorkDirFiles, 1)
	assert.Equal(t, "openclaw.json", exec.WorkDirFiles[0].Name)
	assert.Contains(t, string(exec.WorkDirFiles[0].Content), "github-mcp")
}

func TestHermesPassthroughEnv(t *testing.T) {
	spec, err := Get("hermes")
	require.NoError(t, err)
	exec, err := spec.Command(LaunchContext{MCPEndpoints: mcpEndpoints(), WorkDir: "/w"})
	require.NoError(t, err)
	assert.Empty(t, exec.WorkDirFiles, "hermes consumes the IR, no native file")
	found := false
	for _, e := range exec.Env {
		if len(e) > len("HERMES_MCP_CONFIG=") && e[:len("HERMES_MCP_CONFIG=")] == "HERMES_MCP_CONFIG=" {
			found = true
			assert.Contains(t, e, "github-mcp")
		}
	}
	assert.True(t, found, "HERMES_MCP_CONFIG env present")
}

func TestNoEndpointsNoFiles(t *testing.T) {
	for _, flavor := range []string{"opencode", "openclaw", "hermes"} {
		spec, err := Get(flavor)
		require.NoError(t, err)
		exec, err := spec.Command(LaunchContext{WorkDir: "/w"})
		require.NoError(t, err)
		assert.Empty(t, exec.WorkDirFiles, "%s renders nothing without endpoints", flavor)
	}
}

// TestOpenCodeBYOEndpointRendersProviderBlock (ISI-4188 gap 2): a resolved
// BYO model endpoint rides opencode.json as a provider.<ksquad-byo> block
// (@ai-sdk/openai-compatible + baseURL + models entry) and the --model flag
// addresses it as ksquad-byo/<model> — opencode v1.18.27 does not resolve
// providers from OPENAI_BASE_URL env. With MCP endpoints present both halves
// merge into ONE opencode.json; the endpoint token never lands in the file.
func TestOpenCodeBYOEndpointRendersProviderBlock(t *testing.T) {
	spec, err := Get("opencode")
	require.NoError(t, err)

	exec, err := spec.Command(LaunchContext{
		ModelRoute:   a2a.ModelRoute{Endpoint: "http://10.0.0.185:11434/v1", Model: "qwen3.8:latest", Token: "route-token"},
		MCPEndpoints: mcpEndpoints(),
		WorkDir:      "/w",
	})
	require.NoError(t, err)

	// --model addresses the rendered provider block.
	require.Len(t, exec.WorkDirFiles, 1)
	f := exec.WorkDirFiles[0]
	assert.Equal(t, "opencode.json", f.Name)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(f.Content, &doc))

	provider, ok := doc["provider"].(map[string]any)
	require.True(t, ok, "provider block missing: %s", f.Content)
	byo, ok := provider[capability.OpenCodeBYOProviderID].(map[string]any)
	require.True(t, ok, "provider.%s missing", capability.OpenCodeBYOProviderID)
	assert.Equal(t, "@ai-sdk/openai-compatible", byo["npm"])
	opts := byo["options"].(map[string]any)
	assert.Equal(t, "http://10.0.0.185:11434/v1", opts["baseURL"])
	models := byo["models"].(map[string]any)
	assert.Contains(t, models, "qwen3.8:latest")

	// MCP section merges into the same document.
	mcp, ok := doc["mcp"].(map[string]any)
	require.True(t, ok, "mcp section lost in merged render")
	assert.Contains(t, mcp, "github-mcp")

	// Token never lands in the persisted config (ADR-045 D5).
	assert.NotContains(t, string(f.Content), "route-token", "no literal credential")

	// The --model flag rides the provider prefix.
	found := false
	for i, a := range exec.Args {
		if a == "--model" && i+1 < len(exec.Args) {
			found = true
			assert.Equal(t, capability.OpenCodeBYOProviderID+"/qwen3.8:latest", exec.Args[i+1])
		}
	}
	assert.True(t, found, "--model flag present")
}

// TestOpenCodeBYOOnlyStillRenders (ISI-4188 gap 2): a BYO endpoint with NO
// MCP servers still renders opencode.json (provider-only) — the file is not
// gated on MCP endpoints anymore.
func TestOpenCodeBYOOnlyStillRenders(t *testing.T) {
	spec, err := Get("opencode")
	require.NoError(t, err)
	exec, err := spec.Command(LaunchContext{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "llama3.1"},
		WorkDir:    "/w",
	})
	require.NoError(t, err)
	require.Len(t, exec.WorkDirFiles, 1)
	assert.Equal(t, "opencode.json", exec.WorkDirFiles[0].Name)
	assert.Contains(t, string(exec.WorkDirFiles[0].Content), capability.OpenCodeBYOProviderID)
}

// TestOpenCodeNoRouteKeepsBareModel (ISI-4188 gap 2, regression guard): no
// BYO endpoint → no provider prefix on --model and no provider block (the
// vendor-default wire is unchanged).
func TestOpenCodeNoRouteKeepsBareModel(t *testing.T) {
	spec, err := Get("opencode")
	require.NoError(t, err)
	exec, err := spec.Command(LaunchContext{Model: "claude-sonnet-4", WorkDir: "/w"})
	require.NoError(t, err)
	assert.Empty(t, exec.WorkDirFiles)
	for i, a := range exec.Args {
		if a == "--model" && i+1 < len(exec.Args) {
			assert.Equal(t, "claude-sonnet-4", exec.Args[i+1])
		}
	}
}
