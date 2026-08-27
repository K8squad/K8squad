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
