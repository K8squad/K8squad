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

package run

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/controller/team"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// searchEps returns a resolved set carrying the injected built-in memory-search
// endpoint (as the capability gate injects it: no credential ref — the assembler
// wires it). Reuses the authoringRun identity (asm1 / squad-a / john? — decomposer).
func searchEps() []capability.Endpoint {
	return []capability.Endpoint{
		{Name: "github-mcp", Transport: "streamable-http", URL: "https://gh"},
		{Name: team.SearchMCPServerName, Transport: "streamable-http", URL: "http://memory/mcp"},
	}
}

// ISI-5276 (WS-B): the search endpoint — injected for EVERY run — binds a per-run
// token Secret so the sandbox can authenticate work_item_search. The team claim is
// the Team's tenancy-root UID (ISI-5189 discipline), never the CR name.
func TestEnsureSearchToken(t *testing.T) {
	minter := testMinter(t)

	t.Run("binds + mints with the Team UID scope (not the CR name)", func(t *testing.T) {
		run := authoringRun() // a plain ticket run (no discussion label, no grant)
		c := newAuthoringClient(t, run, teamCR("squad-a", "team-uid-squad-a"))
		asm := &Assembler{Client: c, Minter: minter}

		eps := searchEps()
		asm.bindSearchCredential(run, eps)

		ep := searchEndpoint(eps)
		require.NotNil(t, ep)
		require.NotNil(t, ep.CredentialSecretRef)
		assert.Equal(t, "asm1-search-token", ep.CredentialSecretRef.Name)
		assert.Equal(t, []string{capability.CredentialEnvName(team.SearchMCPServerName)}, ep.EnvNames)

		require.NoError(t, asm.ensureSearchToken(context.Background(), run, eps))

		var sec corev1.Secret
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: asmRunNS, Name: "asm1-search-token"}, &sec))
		tokBytes, ok := sec.Data[authoringTokenSecretKey]
		require.True(t, ok)

		claims, err := minter.Verify(string(tokBytes))
		require.NoError(t, err)
		assert.Equal(t, "team-uid-squad-a", claims.TeamID, "team scope must be the tenancy-root UID (squad_id::uuid), not the CR name")
		assert.NotEqual(t, "squad-a", claims.TeamID)
		assert.Equal(t, "henrik", claims.Principal)
		assert.Equal(t, "decomposer", claims.AgentID)
		assert.Equal(t, "uid-asm1", claims.RunID)
		assert.Equal(t, []string{capability.CapabilitySearch}, claims.Capabilities)
	})

	t.Run("missing Team CR fails closed (no token minted)", func(t *testing.T) {
		run := authoringRun()
		c := newAuthoringClient(t, run) // Team CR absent
		asm := &Assembler{Client: c, Minter: minter}

		eps := searchEps()
		asm.bindSearchCredential(run, eps)
		err := asm.ensureSearchToken(context.Background(), run, eps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolve team")

		gotErr := c.Get(context.Background(), types.NamespacedName{Namespace: asmRunNS, Name: "asm1-search-token"}, &corev1.Secret{})
		assert.True(t, apierrors.IsNotFound(gotErr), "no Secret written on fail-closed")
	})

	t.Run("no search endpoint injected (fail-open namespace) is a no-op", func(t *testing.T) {
		run := authoringRun()
		c := newAuthoringClient(t, run, teamCR("squad-a", "team-uid-squad-a"))
		asm := &Assembler{Client: c, Minter: minter}
		eps := []capability.Endpoint{{Name: "github-mcp"}} // no search endpoint injected
		asm.bindSearchCredential(run, eps)
		require.NoError(t, asm.ensureSearchToken(context.Background(), run, eps))
		err := c.Get(context.Background(), types.NamespacedName{Namespace: asmRunNS, Name: "asm1-search-token"}, &corev1.Secret{})
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("no minter leaves the endpoint credential-less (inert)", func(t *testing.T) {
		asm := &Assembler{} // Minter nil
		eps := searchEps()
		asm.bindSearchCredential(authoringRun(), eps)
		assert.Nil(t, searchEndpoint(eps).CredentialSecretRef)
	})
}
