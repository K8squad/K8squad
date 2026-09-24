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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mcpServer(name string, mutate func(*api.MCPServer)) *api.MCPServer {
	s := &api.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: runNS},
		Spec:       api.MCPServerSpec{Transport: api.MCPTransportStreamableHTTP, Endpoint: "https://mcp.example/" + name},
		Status: api.MCPServerStatus{
			ObservedTools: []string{"create_issue", "list_issues", "create_pull_request"},
		},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func TestResolveMCPMissingServerFailsClosed(t *testing.T) {
	reqs := &Requirements{
		MCPRefs:    []api.ObjectRef{{Name: "ghost"}},
		MCPSources: map[string]string{runNS + "/ghost": runNS + "/some-skill"},
	}
	_, _, err := ResolveMCP(context.Background(), capClient(t), newRun(), reqs, GrantSet{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
	assert.Contains(t, err.Error(), "some-skill") // actionable: names the demander
}

func TestResolveMCPStaleDiscoveryFailsClosed(t *testing.T) {
	server := mcpServer("github-mcp", func(s *api.MCPServer) { s.Status.ObservedTools = nil })
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: "github-mcp"}}}
	_, _, err := ResolveMCP(context.Background(), capClient(t, server), newRun(), reqs, GrantSet{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "observedTools is empty")
}

func TestResolveMCPEmptyAllowGrantsAllObserved(t *testing.T) {
	server := mcpServer("github-mcp", nil)
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: "github-mcp"}}}
	eps, servers, err := ResolveMCP(context.Background(), capClient(t, server), newRun(), reqs, GrantSet{})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.ElementsMatch(t, []string{"create_issue", "list_issues", "create_pull_request"}, eps[0].AllowTools)
	require.Len(t, servers, 1)
	assert.Equal(t, "github-mcp", servers[0].Name)
}

func TestResolveMCPAllowGlobsNarrowObserved(t *testing.T) {
	server := mcpServer("github-mcp", func(s *api.MCPServer) {
		s.Spec.ToolFilter = &api.MCPToolFilter{Allow: []string{"create_*"}}
	})
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: "github-mcp"}}}
	eps, _, err := ResolveMCP(context.Background(), capClient(t, server), newRun(), reqs, GrantSet{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"create_issue", "create_pull_request"}, eps[0].AllowTools)
}

func TestResolveMCPDenyGlobSubtracts(t *testing.T) {
	server := mcpServer("github-mcp", func(s *api.MCPServer) {
		s.Spec.ToolFilter = &api.MCPToolFilter{
			Allow: []string{"create_*", "list_*"},
			Deny:  []string{"create_pull_request"},
		}
	})
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: "github-mcp"}}}
	eps, _, err := ResolveMCP(context.Background(), capClient(t, server), newRun(), reqs, GrantSet{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"create_issue", "list_issues"}, eps[0].AllowTools)
}

func TestResolveMCPGlobOverlapNullifiesGrantFailsClosed(t *testing.T) {
	// allow create_* vs deny create_issue applied to a surface of exactly
	// create_issue — the CEL literal-overlap guard cannot see this; the
	// empty effective set fails closed here (ADR-042).
	server := mcpServer("github-mcp", func(s *api.MCPServer) {
		s.Status.ObservedTools = []string{"create_issue"}
		s.Spec.ToolFilter = &api.MCPToolFilter{
			Allow: []string{"create_*"},
			Deny:  []string{"create_issue"},
		}
	})
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: "github-mcp"}}}
	_, _, err := ResolveMCP(context.Background(), capClient(t, server), newRun(), reqs, GrantSet{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "effective allow set is empty")
}

func TestResolveMCPAllowMatchingNothingObservedFailsClosed(t *testing.T) {
	server := mcpServer("github-mcp", func(s *api.MCPServer) {
		s.Spec.ToolFilter = &api.MCPToolFilter{Allow: []string{"destroy_*"}}
	})
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: "github-mcp"}}}
	_, _, err := ResolveMCP(context.Background(), capClient(t, server), newRun(), reqs, GrantSet{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "effective allow set is empty")
}

func TestResolveMCPStdioShapeAndCredentialName(t *testing.T) {
	server := mcpServer("gh-stdio", func(s *api.MCPServer) {
		s.Spec.Transport = api.MCPTransportStdio
		s.Spec.Endpoint = ""
		s.Spec.Command = "gh-mcp-serve"
		s.Spec.Args = []string{"--org", "acme"}
		s.Spec.Image = "ghcr.io/k8squad/mcp/gh:1.4"
		s.Spec.CredentialSecretRef = &api.SecretRef{Name: "gh-token"}
	})
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: "gh-stdio"}}}
	eps, _, err := ResolveMCP(context.Background(), capClient(t, server), newRun(), reqs, GrantSet{})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	ep := eps[0]
	assert.Equal(t, "gh-mcp-serve", ep.Command)
	assert.Equal(t, []string{"--org", "acme"}, ep.Args)
	assert.Equal(t, "ghcr.io/k8squad/mcp/gh:1.4", ep.Image)
	assert.Equal(t, []string{"KSQUAD_MCP_GH_STDIO_TOKEN"}, ep.EnvNames)
	// The IR carries the secret NAME, never material.
	require.NotNil(t, ep.CredentialSecretRef)
	assert.Equal(t, "gh-token", ep.CredentialSecretRef.Name)
}

func TestResolveMCPHTTPMissingEndpointDriftFailsClosed(t *testing.T) {
	server := mcpServer("broken", func(s *api.MCPServer) { s.Spec.Endpoint = "" })
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: "broken"}}}
	_, _, err := ResolveMCP(context.Background(), capClient(t, server), newRun(), reqs, GrantSet{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.endpoint is empty")
}

func TestGlobMatchDialect(t *testing.T) {
	assert.True(t, globMatch("*", "anything"))
	assert.True(t, globMatch("create_*", "create_issue"))
	assert.False(t, globMatch("create_*", "list_issues"))
	assert.True(t, globMatch("create_issue", "create_issue"))
	assert.False(t, globMatch("create_issue", "create_pull_request"))
}

func TestCredentialEnvNameNormalization(t *testing.T) {
	assert.Equal(t, "KSQUAD_MCP_GITHUB_MCP_TOKEN", CredentialEnvName("github-mcp"))
	assert.Equal(t, "KSQUAD_MCP_DT_MCP_TOKEN", CredentialEnvName("dt.mcp"))
}

// grantedAuthor is the resolved GrantSet a work_item.author-holding Run
// carries (produced by ResolveGrant, S4). Constructed directly here since the
// gate keys only on GrantSet.Has, decoupling S2's tests from Team/Agent wiring.
func grantedAuthor() GrantSet {
	return GrantSet{caps: map[string]struct{}{CapabilityWorkItemAuthor: {}}}
}

// builtinAuthoringServer is a well-formed built-in memory-authoring MCPServer
// in the Run namespace with a seeded observedTools surface (as S1's provisioner
// leaves it).
func builtinAuthoringServer(mutate func(*api.MCPServer)) *api.MCPServer {
	s := &api.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: authoringMCPServerName, Namespace: runNS},
		Spec: api.MCPServerSpec{
			Transport: api.MCPTransportStreamableHTTP,
			Endpoint:  "http://ksquad-memory-authoring." + runNS + ".svc/mcp",
		},
		Status: api.MCPServerStatus{
			ObservedTools: []string{"work_item_create", "work_item_update", "work_item_assign"},
		},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

// ADR-0024a S2 (ISI-4868): a granted-agent Run resolves the built-in authoring
// endpoint even with zero MCPRefs referencing it (auto-injection).
func TestResolveMCPGrantedAgentAutoInjectsBuiltin(t *testing.T) {
	builtin := builtinAuthoringServer(nil)
	reqs := &Requirements{} // zero MCPRefs
	eps, servers, err := ResolveMCP(context.Background(), capClient(t, builtin), newRun(), reqs, grantedAuthor())
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, authoringMCPServerName, eps[0].Name)
	assert.ElementsMatch(t, []string{"work_item_create", "work_item_update", "work_item_assign"}, eps[0].AllowTools)
	require.Len(t, servers, 1)
	assert.Equal(t, authoringMCPServerName, servers[0].Name)
}

// ADR-0024a S2: an ungranted-agent Run does NOT get the endpoint even when the
// built-in MCPServer exists (deny-by-default at the wiring layer).
func TestResolveMCPUngrantedAgentNoBuiltin(t *testing.T) {
	builtin := builtinAuthoringServer(nil)
	reqs := &Requirements{}
	eps, servers, err := ResolveMCP(context.Background(), capClient(t, builtin), newRun(), reqs, GrantSet{})
	require.NoError(t, err)
	assert.Empty(t, eps)
	assert.Empty(t, servers)
}

// ADR-0024a S2: granted, but the built-in surfaced no tools (empty
// observedTools) → fail closed, reusing endpointFor's guard. This proves the
// S1 dependency (seeded observedTools) is load-bearing.
func TestResolveMCPGrantedEmptyObservedFailsClosed(t *testing.T) {
	builtin := builtinAuthoringServer(func(s *api.MCPServer) { s.Status.ObservedTools = nil })
	reqs := &Requirements{}
	_, _, err := ResolveMCP(context.Background(), capClient(t, builtin), newRun(), reqs, grantedAuthor())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "observedTools is empty")
	assert.Contains(t, err.Error(), authoringMCPServerName)
}

// ADR-0024a S2: granted, but the built-in MCPServer CR is missing in the Run
// namespace (S1 not yet reconciled) → fail closed rather than dispatch a
// granted agent with no authoring tools.
func TestResolveMCPGrantedBuiltinMissingFailsClosed(t *testing.T) {
	reqs := &Requirements{}
	_, _, err := ResolveMCP(context.Background(), capClient(t), newRun(), reqs, grantedAuthor())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Contains(t, err.Error(), authoringMCPServerName)
}

// ADR-0024a S2: existing MCPRefs resolution is unaffected by the gate — a
// granted Run that also references other servers resolves both, and the
// injected built-in sorts deterministically with the rest.
func TestResolveMCPGrantedInjectionCoexistsAndSorts(t *testing.T) {
	builtin := builtinAuthoringServer(nil)
	other := mcpServer("zzz-github", nil) // sorts after the built-in
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: "zzz-github"}}}
	eps, servers, err := ResolveMCP(context.Background(), capClient(t, builtin, other), newRun(), reqs, grantedAuthor())
	require.NoError(t, err)
	require.Len(t, eps, 2)
	// Stable, name-sorted ordering: built-in ("k...") precedes "zzz-github".
	assert.Equal(t, authoringMCPServerName, eps[0].Name)
	assert.Equal(t, "zzz-github", eps[1].Name)
	require.Len(t, servers, 2)
	assert.Equal(t, authoringMCPServerName, servers[0].Name)
	assert.Equal(t, "zzz-github", servers[1].Name)
}

// ADR-0024a S2: if a project ALSO references the built-in via MCPRefs, the
// gate must not double-inject — the built-in name is reserved.
func TestResolveMCPGrantedNoDoubleInjectionWhenReferenced(t *testing.T) {
	builtin := builtinAuthoringServer(nil)
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: authoringMCPServerName}}}
	eps, servers, err := ResolveMCP(context.Background(), capClient(t, builtin), newRun(), reqs, grantedAuthor())
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, authoringMCPServerName, eps[0].Name)
	require.Len(t, servers, 1)
}
