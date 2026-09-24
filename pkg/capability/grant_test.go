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

// grantRun builds a Run in runNS referencing a Team and the named agents.
func grantRun(teamName string, agentNames ...string) *api.Run {
	refs := make([]api.ObjectRef, 0, len(agentNames))
	for _, n := range agentNames {
		refs = append(refs, api.ObjectRef{Name: n})
	}
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: runNS, UID: "run-uid-1"},
		Spec: api.RunSpec{
			TeamRef: api.ObjectRef{Name: teamName},
			Agents:  refs,
		},
	}
}

func teamWithGrants(name string, grants ...api.CapabilityGrant) *api.Team {
	return &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: runNS},
		Spec:       api.TeamSpec{Grants: grants},
	}
}

func agentWithRole(name, role string) *api.Agent {
	return &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: runNS},
		Spec:       api.AgentSpec{RoleRef: api.ObjectRef{Name: role}},
	}
}

// TestResolveGrantPresent: the run agent's role is granted work_item.author
// on the owning Team → the resolver returns it (ADR-0024a S4 grant-present).
func TestResolveGrantPresent(t *testing.T) {
	team := teamWithGrants("squad",
		api.CapabilityGrant{Role: "pm", Capabilities: []string{CapabilityWorkItemAuthor}})
	agent := agentWithRole("quill", "pm")
	run := grantRun("squad", "quill")

	got, err := ResolveGrant(context.Background(), capClient(t, team, agent), run)
	require.NoError(t, err)
	assert.True(t, got.Has(CapabilityWorkItemAuthor))
	assert.False(t, got.Empty())
	assert.Equal(t, []string{CapabilityWorkItemAuthor}, got.List())
}

// TestResolveGrantAbsentRoleNotGranted: the run agent's role holds no grant
// on the Team → empty set, deny-by-default (grant-absent read path).
func TestResolveGrantAbsentRoleNotGranted(t *testing.T) {
	team := teamWithGrants("squad",
		api.CapabilityGrant{Role: "pm", Capabilities: []string{CapabilityWorkItemAuthor}})
	agent := agentWithRole("coder", "dev") // dev has no grant
	run := grantRun("squad", "coder")

	got, err := ResolveGrant(context.Background(), capClient(t, team, agent), run)
	require.NoError(t, err)
	assert.False(t, got.Has(CapabilityWorkItemAuthor))
	assert.True(t, got.Empty())
	assert.Nil(t, got.List())
}

// TestResolveGrantNoGrantsOnTeam: a Team with an empty grant store grants
// nothing.
func TestResolveGrantNoGrantsOnTeam(t *testing.T) {
	team := teamWithGrants("squad") // no grants
	agent := agentWithRole("quill", "pm")
	run := grantRun("squad", "quill")

	got, err := ResolveGrant(context.Background(), capClient(t, team, agent), run)
	require.NoError(t, err)
	assert.True(t, got.Empty())
}

// TestResolveGrantMissingTeamFailsClosed: a Run whose Team does not exist
// resolves to the empty set (deny-by-default), NOT an error-open.
func TestResolveGrantMissingTeamFailsClosed(t *testing.T) {
	agent := agentWithRole("quill", "pm")
	run := grantRun("ghost-team", "quill")

	got, err := ResolveGrant(context.Background(), capClient(t, agent), run)
	require.NoError(t, err)
	assert.True(t, got.Empty())
}

// TestResolveGrantMissingAgentIgnored: an agent ref that resolves to no
// Agent CR is skipped (not an error) — its would-be grant simply does not
// contribute.
func TestResolveGrantMissingAgentIgnored(t *testing.T) {
	team := teamWithGrants("squad",
		api.CapabilityGrant{Role: "pm", Capabilities: []string{CapabilityWorkItemAuthor}})
	run := grantRun("squad", "ghost") // agent CR absent

	got, err := ResolveGrant(context.Background(), capClient(t, team), run)
	require.NoError(t, err)
	assert.True(t, got.Empty())
}

// TestResolveGrantGrantForRoleNoAgentHolds: a grant naming a role that no
// dispatched agent holds is ignored (does not grant).
func TestResolveGrantGrantForRoleNoAgentHolds(t *testing.T) {
	team := teamWithGrants("squad",
		api.CapabilityGrant{Role: "pm", Capabilities: []string{CapabilityWorkItemAuthor}})
	agent := agentWithRole("coder", "dev")
	run := grantRun("squad", "coder")

	got, err := ResolveGrant(context.Background(), capClient(t, team, agent), run)
	require.NoError(t, err)
	assert.True(t, got.Empty())
}

// TestResolveGrantUnionAcrossAgents: multiple dispatched agents whose roles
// carry distinct grants union into one set (documented precedence: union).
func TestResolveGrantUnionAcrossAgents(t *testing.T) {
	team := teamWithGrants("squad",
		api.CapabilityGrant{Role: "pm", Capabilities: []string{CapabilityWorkItemAuthor}},
		api.CapabilityGrant{Role: "dev", Capabilities: []string{"extra.cap"}})
	pm := agentWithRole("quill", "pm")
	dev := agentWithRole("coder", "dev")
	run := grantRun("squad", "quill", "coder")

	got, err := ResolveGrant(context.Background(), capClient(t, team, pm, dev), run)
	require.NoError(t, err)
	assert.True(t, got.Has(CapabilityWorkItemAuthor))
	assert.True(t, got.Has("extra.cap"))
	assert.Equal(t, []string{"extra.cap", CapabilityWorkItemAuthor}, got.List())
}

// TestGrantSetZeroValueDenies: the zero-value GrantSet is deny-by-default.
func TestGrantSetZeroValueDenies(t *testing.T) {
	var g GrantSet
	assert.False(t, g.Has(CapabilityWorkItemAuthor))
	assert.True(t, g.Empty())
	assert.Nil(t, g.List())
}
