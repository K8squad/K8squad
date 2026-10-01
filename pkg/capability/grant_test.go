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

// roleCR builds a Role CR in runNS, optionally a coordinator.
func roleCR(name string, coordinator bool) *api.Role {
	return &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: runNS},
		Spec:       api.RoleSpec{Coordinator: coordinator},
	}
}

// TestResolveGrantCoordinatorDefault (ISI-5223): a dispatched agent whose Role
// is a coordinator is granted work_item.author by DEFAULT — even with no Team
// grant store — so a PM/coordinator can orchestrate out of the box.
func TestResolveGrantCoordinatorDefault(t *testing.T) {
	team := teamWithGrants("squad") // no explicit grants
	role := roleCR("role-manager", true)
	agent := agentWithRole("john", "role-manager")
	run := grantRun("squad", "john")

	got, err := ResolveGrant(context.Background(), capClient(t, team, role, agent), run)
	require.NoError(t, err)
	assert.True(t, got.Has(CapabilityWorkItemAuthor), "coordinator is author-capable by default")
}

// TestResolveGrantCoordinatorDefaultNoTeam (ISI-5223): the coordinator default
// derives from the Role CR, so it applies even when the Team CR is absent.
func TestResolveGrantCoordinatorDefaultNoTeam(t *testing.T) {
	role := roleCR("role-manager", true)
	agent := agentWithRole("john", "role-manager")
	run := grantRun("ghost-team", "john")

	got, err := ResolveGrant(context.Background(), capClient(t, role, agent), run)
	require.NoError(t, err)
	assert.True(t, got.Has(CapabilityWorkItemAuthor))
}

// TestResolveGrantNonCoordinatorNoDefault (ISI-5223): a non-coordinator Role
// gets NO default grant — deny-by-default preserved for ICs.
func TestResolveGrantNonCoordinatorNoDefault(t *testing.T) {
	team := teamWithGrants("squad")
	role := roleCR("role-implementer", false)
	agent := agentWithRole("ada", "role-implementer")
	run := grantRun("squad", "ada")

	got, err := ResolveGrant(context.Background(), capClient(t, team, role, agent), run)
	require.NoError(t, err)
	assert.True(t, got.Empty(), "non-coordinator role is deny-by-default")
}

// TestResolveGrantCoordinatorDefaultUnionsWithTeamGrants (ISI-5223): the
// coordinator default unions with explicit Team.Spec.Grants for other roles.
func TestResolveGrantCoordinatorDefaultUnionsWithTeamGrants(t *testing.T) {
	team := teamWithGrants("squad",
		api.CapabilityGrant{Role: "role-implementer", Capabilities: []string{"extra.cap"}})
	mgrRole := roleCR("role-manager", true)
	devRole := roleCR("role-implementer", false)
	mgr := agentWithRole("john", "role-manager")
	dev := agentWithRole("ada", "role-implementer")
	run := grantRun("squad", "john", "ada")

	got, err := ResolveGrant(context.Background(), capClient(t, team, mgrRole, devRole, mgr, dev), run)
	require.NoError(t, err)
	assert.True(t, got.Has(CapabilityWorkItemAuthor), "coordinator default")
	assert.True(t, got.Has("extra.cap"), "explicit team grant for the IC role")
}

// roleCRMode builds a coordinator Role CR with an explicit CoordinatorMode.
func roleCRMode(name, mode string) *api.Role {
	return &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: runNS},
		Spec:       api.RoleSpec{Coordinator: true, CoordinatorMode: mode},
	}
}

// TestResolveGrantCoordinatorProposeMode (ISI-5282): a coordinator Role whose
// CoordinatorMode is "propose" gets BOTH work_item.author AND the derived
// coordinator.propose grant, so the authoring edge raises proposals.
func TestResolveGrantCoordinatorProposeMode(t *testing.T) {
	team := teamWithGrants("squad")
	role := roleCRMode("role-manager", "propose")
	agent := agentWithRole("john", "role-manager")
	run := grantRun("squad", "john")

	got, err := ResolveGrant(context.Background(), capClient(t, team, role, agent), run)
	require.NoError(t, err)
	assert.True(t, got.Has(CapabilityWorkItemAuthor), "propose coordinator still authors")
	assert.True(t, got.Has(CapabilityCoordinatorPropose), "propose mode is a derived grant")
	assert.Equal(t, []string{CapabilityCoordinatorPropose, CapabilityWorkItemAuthor}, got.List())
}

// TestResolveGrantCoordinatorAutoModeNoPropose (ISI-5282): a coordinator in auto
// mode (explicit "auto") gets work_item.author but NOT coordinator.propose.
func TestResolveGrantCoordinatorAutoModeNoPropose(t *testing.T) {
	role := roleCRMode("role-manager", "auto")
	agent := agentWithRole("john", "role-manager")
	run := grantRun("squad", "john")

	got, err := ResolveGrant(context.Background(), capClient(t, role, agent), run)
	require.NoError(t, err)
	assert.True(t, got.Has(CapabilityWorkItemAuthor))
	assert.False(t, got.Has(CapabilityCoordinatorPropose), "auto mode gets no propose grant")
}

// TestResolveGrantCoordinatorEmptyModeDefaultsAuto (ISI-5282): an unset
// CoordinatorMode defaults to auto — author-capable, no propose gate.
func TestResolveGrantCoordinatorEmptyModeDefaultsAuto(t *testing.T) {
	role := roleCR("role-manager", true) // Coordinator=true, CoordinatorMode=""
	agent := agentWithRole("john", "role-manager")
	run := grantRun("squad", "john")

	got, err := ResolveGrant(context.Background(), capClient(t, role, agent), run)
	require.NoError(t, err)
	assert.True(t, got.Has(CapabilityWorkItemAuthor))
	assert.False(t, got.Has(CapabilityCoordinatorPropose), "empty mode defaults to auto")
}

// TestResolveGrantNonCoordinatorNeverPropose (ISI-5282): a non-coordinator role
// never gets coordinator.propose, even if (impossibly) its mode were set.
func TestResolveGrantNonCoordinatorNeverPropose(t *testing.T) {
	role := &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "role-implementer", Namespace: runNS},
		Spec:       api.RoleSpec{Coordinator: false, CoordinatorMode: "propose"},
	}
	agent := agentWithRole("ada", "role-implementer")
	run := grantRun("squad", "ada")

	got, err := ResolveGrant(context.Background(), capClient(t, role, agent), run)
	require.NoError(t, err)
	assert.False(t, got.Has(CapabilityCoordinatorPropose))
	assert.True(t, got.Empty(), "non-coordinator is deny-by-default regardless of mode")
}

// TestGrantSetZeroValueDenies: the zero-value GrantSet is deny-by-default.
func TestGrantSetZeroValueDenies(t *testing.T) {
	var g GrantSet
	assert.False(t, g.Has(CapabilityWorkItemAuthor))
	assert.True(t, g.Empty())
	assert.Nil(t, g.List())
}
