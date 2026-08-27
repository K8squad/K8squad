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

package v1alpha1

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Story A1 AC1–AC3: admitted fixtures and each rejection class, with the
// actionable message asserted by substring so the fix text stays honest.

func stdioServer() *MCPServer {
	return &MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "local-tool", Namespace: "bmad-squad"},
		Spec: MCPServerSpec{
			Transport: MCPTransportStdio,
			Command:   "/tools/server",
			Args:      []string{"--stdio"},
			Image:     "ghcr.io/k8squad/mcp/local-tool:1.0",
		},
	}
}

func httpServer() *MCPServer {
	return &MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github-mcp", Namespace: "bmad-squad"},
		Spec: MCPServerSpec{
			Transport:           MCPTransportStreamableHTTP,
			Endpoint:            "https://api.githubcopilot.com/mcp/",
			Headers:             map[string]string{"X-Org": "k8squad"},
			CredentialSecretRef: &SecretRef{Name: "github-mcp-token", Key: "token"},
			ToolFilter: &MCPToolFilter{
				Allow: []string{"create_pull_request", "list_issues"},
				Deny:  []string{"delete_*"},
			},
			Discovery: &MCPServerDiscovery{IntervalMinutes: int32Ptr(10)},
		},
	}
}

func TestMCPServerAdmittedFixtures(t *testing.T) {
	ctx := context.Background()
	for name, srv := range map[string]*MCPServer{"stdio": stdioServer(), "streamable-http": httpServer()} {
		w, err := srv.ValidateCreate(ctx, srv)
		require.NoError(t, err, name)
		assert.Empty(t, w, name)
	}
}

func TestMCPServerTransportPairing(t *testing.T) {
	ctx := context.Background()

	withEndpoint := stdioServer()
	withEndpoint.Spec.Endpoint = "https://example.invalid/mcp"
	_, err := withEndpoint.ValidateCreate(ctx, withEndpoint)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.endpoint is forbidden when transport is stdio")

	noCommand := stdioServer()
	noCommand.Spec.Command = ""
	_, err = noCommand.ValidateCreate(ctx, noCommand)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.command is required when transport is stdio")

	noEndpoint := httpServer()
	noEndpoint.Spec.Endpoint = ""
	_, err = noEndpoint.ValidateCreate(ctx, noEndpoint)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.endpoint is required when transport is streamable-http")

	withCommand := httpServer()
	withCommand.Spec.Command = "/tools/server"
	_, err = withCommand.ValidateCreate(ctx, withCommand)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.command is forbidden when transport is streamable-http")

	badScheme := httpServer()
	badScheme.Spec.Endpoint = "ftp://example.invalid/mcp"
	_, err = badScheme.ValidateCreate(ctx, badScheme)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must use the http or https scheme")
}

func TestMCPServerToolFilterOverlapRejected(t *testing.T) {
	ctx := context.Background()

	srv := httpServer()
	srv.Spec.ToolFilter = &MCPToolFilter{
		Allow: []string{"create_pull_request", "list_issues"},
		Deny:  []string{"create_pull_request", "delete_*"},
	}
	_, err := srv.ValidateCreate(ctx, srv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"create_pull_request" appears in both allow and deny`)
}

func TestMCPServerSecretLookingHeadersRejected(t *testing.T) {
	ctx := context.Background()

	for header, want := range map[string]string{
		"Authorization":  "credentialSecretRef",
		"Cookie":         "credentialSecretRef",
		"X-Api-Key":      "credentialSecretRef",
		"X-Github-Token": "credentialSecretRef",
	} {
		srv := httpServer()
		srv.Spec.Headers[header] = "definitely-a-secret"
		_, err := srv.ValidateCreate(ctx, srv)
		require.Error(t, err, header)
		assert.Contains(t, err.Error(), want, header)
	}
}

func TestMCPServerReservedNamesRejected(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"workspace", "computer-use", "claude-in-chrome", "__proto__"} {
		srv := httpServer()
		srv.Name = name
		_, err := srv.ValidateCreate(ctx, srv)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "reserved by the runtime MCP clients")
	}
}

func TestMCPServerCredentialAndEgressRefShape(t *testing.T) {
	ctx := context.Background()

	badCred := httpServer()
	badCred.Spec.CredentialSecretRef = &SecretRef{Name: ""}
	_, err := badCred.ValidateCreate(ctx, badCred)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credentialSecretRef.name must not be empty")

	badEgress := httpServer()
	badEgress.Spec.EgressRef = &ObjectRef{Name: ""}
	_, err = badEgress.ValidateCreate(ctx, badEgress)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "egressRef.name must not be empty")
}

func TestMCPServerWebhookMethods(t *testing.T) {
	ctx := context.Background()
	v := &MCPServer{}

	// Update mirrors create; delete is free.
	w, err := v.ValidateUpdate(ctx, nil, httpServer())
	require.NoError(t, err)
	assert.Empty(t, w)

	w, err = v.ValidateDelete(ctx, httpServer())
	require.NoError(t, err)
	assert.Empty(t, w)

	// The controller-runtime 0.24 typed validator surface makes a
	// wrong-kind object unrepresentable (ValidateCreate takes
	// *MCPServer); a nil object still fails loudly rather than panicking.
	_, err = v.ValidateCreate(ctx, nil)
	require.ErrorContains(t, err, "expected an MCPServer object")
}
