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
	"github.com/K8squad/K8squad/pkg/toolchain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildManifestRecordsEnvelopeWithoutSecretMaterial(t *testing.T) {
	m := BuildManifest(resolvedToolchains(), []Endpoint{stdioEndpoint(), httpEndpoint()})

	require.Len(t, m.Toolchains, 2)
	assert.Equal(t, "kubectl", m.Toolchains[0].Name)
	assert.Equal(t, "ghcr.io/k8squad/toolchains/kubectl:1.31", m.Toolchains[0].Image)

	require.Len(t, m.MCPEndpoints, 2)
	byName := map[string]api.ResolvedMCPEndpoint{}
	for _, ep := range m.MCPEndpoints {
		byName[ep.Name] = ep
	}
	gh := byName["gh-stdio"]
	assert.True(t, gh.Sidecar)
	http := byName["github-mcp"]
	assert.False(t, http.Sidecar)
	assert.Equal(t, []string{"list_issues"}, http.AllowTools)
	require.NotNil(t, http.CredentialSecretRef)
	assert.Equal(t, "github-token", http.CredentialSecretRef.Name)

	assert.NotEmpty(t, m.CapabilityHash)
	assert.Len(t, m.CapabilityHash, 64) // sha256 hex
}

func TestManifestHashDeterministicAndSensitive(t *testing.T) {
	a := BuildManifest(resolvedToolchains(), scopedEndpoints())
	b := BuildManifest(resolvedToolchains(), scopedEndpoints())
	assert.Equal(t, a.CapabilityHash, b.CapabilityHash, "identical envelopes hash identically")

	// Any envelope change (a version pin) changes the hash → new pool key.
	bumped := resolvedToolchains()
	bumped[1].Version = "2.63"
	c := BuildManifest(bumped, scopedEndpoints())
	assert.NotEqual(t, a.CapabilityHash, c.CapabilityHash)

	// A filter change changes the hash.
	narrowed := scopedEndpoints()
	narrowed[0].AllowTools = narrowed[0].AllowTools[:1]
	d := BuildManifest(resolvedToolchains(), narrowed)
	assert.NotEqual(t, a.CapabilityHash, d.CapabilityHash)

	// The hash itself does not feed the hash (self-reference elision).
	assert.Equal(t, a.CapabilityHash, HashManifest(a))
}

func TestEmptyManifestStillHashes(t *testing.T) {
	m := BuildManifest(nil, nil)
	assert.Empty(t, m.Toolchains)
	assert.Empty(t, m.MCPEndpoints)
	assert.NotEmpty(t, m.CapabilityHash)
}

func TestCheckEgress(t *testing.T) {
	run := newRun()

	t.Run("stdio rides pod policy — no check", func(t *testing.T) {
		s := mcpServer("s", func(x *api.MCPServer) {
			x.Spec.Transport = api.MCPTransportStdio
			x.Spec.Endpoint = ""
			x.Spec.Command = "x"
			x.Spec.EgressRef = &api.ObjectRef{Name: "missing"} // ignored for stdio
		})
		require.NoError(t, CheckEgress(context.Background(), capClient(t), run, s))
	})

	t.Run("http without egressRef passes", func(t *testing.T) {
		require.NoError(t, CheckEgress(context.Background(), capClient(t), run, mcpServer("s", nil)))
	})

	t.Run("http with missing policy fails closed", func(t *testing.T) {
		s := mcpServer("s", func(x *api.MCPServer) {
			x.Spec.EgressRef = &api.ObjectRef{Name: "github-egress"}
		})
		err := CheckEgress(context.Background(), capClient(t), run, s)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
	})

	t.Run("http with existing policy passes", func(t *testing.T) {
		s := mcpServer("s", func(x *api.MCPServer) {
			x.Spec.EgressRef = &api.ObjectRef{Name: "github-egress"}
		})
		policy := &api.EgressPolicy{ObjectMeta: metav1.ObjectMeta{Name: "github-egress", Namespace: runNS}}
		require.NoError(t, CheckEgress(context.Background(), capClient(t, s, policy), run, s))
	})

	t.Run("EgressAllowed=False fails closed", func(t *testing.T) {
		s := mcpServer("s", func(x *api.MCPServer) {
			x.Spec.EgressRef = &api.ObjectRef{Name: "github-egress"}
			x.Status.Conditions = []metav1.Condition{{
				Type:   api.MCPServerConditionEgressAllowed,
				Status: metav1.ConditionFalse,
				Reason: "PolicyMismatch",
			}}
		})
		policy := &api.EgressPolicy{ObjectMeta: metav1.ObjectMeta{Name: "github-egress", Namespace: runNS}}
		err := CheckEgress(context.Background(), capClient(t, s, policy), run, s)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "EgressAllowed=False")
	})
}

// Compile-time: the resolver contract the assembler relies on.
var _ = toolchain.Resolver{}
