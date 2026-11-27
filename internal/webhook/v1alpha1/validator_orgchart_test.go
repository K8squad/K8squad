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

package webhook

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ISI-7946 WS-A: the mandatory org chart at Team admission (GuardTeamOrgChart).

func orgRole(name, reportsTo string, coordinator bool, process ...ksquadv1alpha1.ProcessStage) *ksquadv1alpha1.Role {
	spec := ksquadv1alpha1.RoleSpec{Coordinator: coordinator}
	if reportsTo != "" {
		spec.ReportsTo = &ksquadv1alpha1.ObjectRef{Name: reportsTo}
	}
	if len(process) > 0 {
		spec.Process = process
	}
	return &ksquadv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
}

func orgAgent(name, role string) *ksquadv1alpha1.Agent {
	return &ksquadv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       ksquadv1alpha1.AgentSpec{RoleRef: ksquadv1alpha1.ObjectRef{Name: role}},
	}
}

func orgTeam(agentNames ...string) *ksquadv1alpha1.Team {
	refs := make([]ksquadv1alpha1.ObjectRef, 0, len(agentNames))
	for _, a := range agentNames {
		refs = append(refs, ksquadv1alpha1.ObjectRef{Name: a})
	}
	return &ksquadv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-alpha", Namespace: ns},
		Spec:       ksquadv1alpha1.TeamSpec{Agents: refs, NamespaceStrategy: "dedicated"},
	}
}

func orgValidator(t *testing.T, objs ...client.Object) *CrossRefValidator {
	t.Helper()
	return newValidator(t, objs)
}

func TestTeamOrgChartSpecEdgesAdmit(t *testing.T) {
	ctx := context.Background()
	v := orgValidator(t,
		orgRole("ceo", "", true),
		orgRole("product-manager", "ceo", false, ksquadv1alpha1.ProcessStage{Stage: "brainstorm", RoleRef: ksquadv1alpha1.ObjectRef{Name: "brainstormer"}}),
		orgRole("brainstormer", "product-manager", false),
		orgAgent("sam", "ceo"), orgAgent("john", "product-manager"), orgAgent("mary", "brainstormer"),
	)
	assert.NoError(t, toInvalid("Team", "squad-alpha", v.ValidateTeam(ctx, orgTeam("sam", "john", "mary"))),
		"conforming spec-edge hierarchy with an in-team manager process must admit")
}

func TestTeamOrgChartLegacyLabelsAdmit(t *testing.T) {
	ctx := context.Background()
	ceo := orgRole("ceo", "", false)
	pm := orgRole("product-manager", "", false)
	ic := orgRole("brainstormer", "", false)
	pm.Labels = map[string]string{ksquadv1alpha1.LabelReportsTo: "ceo"}
	ic.Labels = map[string]string{ksquadv1alpha1.LabelReportsTo: "product-manager"}
	v := orgValidator(t, ceo, pm, ic,
		orgAgent("sam", "ceo"), orgAgent("john", "product-manager"), orgAgent("mary", "brainstormer"))
	assert.NoError(t, toInvalid("Team", "squad-alpha", v.ValidateTeam(ctx, orgTeam("sam", "john", "mary"))),
		"legacy label-only chart must keep admitting (compat)")
}

func TestTeamOrgChartFlatSynthesis(t *testing.T) {
	ctx := context.Background()
	// flat + unique coordinator → synthesized root
	v := orgValidator(t,
		orgRole("boss", "", true), orgRole("worker", "", false),
		orgAgent("b", "boss"), orgAgent("w", "worker"))
	assert.NoError(t, toInvalid("Team", "squad-alpha", v.ValidateTeam(ctx, orgTeam("b", "w"))),
		"flat team with a unique coordinator gets the synthesized default chart")
	// flat + 'ceo' role name → synthesized root
	v2 := orgValidator(t, orgRole("ceo", "", false), orgRole("worker", "", false),
		orgAgent("c", "ceo"), orgAgent("w2", "worker"))
	assert.NoError(t, toInvalid("Team", "squad-alpha", v2.ValidateTeam(ctx, orgTeam("c", "w2"))),
		"flat team with a 'ceo' Role gets the synthesized default chart")
	// single-role flat team is trivially conforming
	v3 := orgValidator(t, orgRole("coder", "", false), orgAgent("a", "coder"))
	assert.NoError(t, toInvalid("Team", "squad-alpha", v3.ValidateTeam(ctx, orgTeam("a"))),
		"a one-role flat team has exactly one root")
}

func TestTeamOrgChartRejects(t *testing.T) {
	ctx := context.Background()
	ps := func(stage, role string) ksquadv1alpha1.ProcessStage {
		return ksquadv1alpha1.ProcessStage{Stage: stage, RoleRef: ksquadv1alpha1.ObjectRef{Name: role}}
	}
	cases := []struct {
		name string
		objs []client.Object
		team []string
		want string
	}{
		{
			name: "flat-no-root",
			objs: []client.Object{orgRole("r1", "", false), orgRole("r2", "", false), orgAgent("a1", "r1"), orgAgent("a2", "r2")},
			team: []string{"a1", "a2"},
			want: "identifiable root",
		},
		{
			name: "two-roots",
			objs: []client.Object{orgRole("ceo", "", false), orgRole("a", "ceo", false), orgRole("orphan", "", false), orgAgent("c", "ceo"), orgAgent("x", "a"), orgAgent("y", "orphan")},
			team: []string{"c", "x", "y"},
			want: "exactly one root",
		},
		{
			name: "cycle",
			objs: []client.Object{orgRole("a", "b", false), orgRole("b", "a", false), orgAgent("x", "a"), orgAgent("y", "b")},
			team: []string{"x", "y"},
			want: "cyclic",
		},
		{
			name: "dangling-manager",
			objs: []client.Object{orgRole("a", "ghost", false), orgRole("ceo", "", false), orgAgent("x", "a"), orgAgent("c", "ceo")},
			team: []string{"x", "c"},
			want: "not part of this team's composition",
		},
		{
			name: "process-role-not-in-team",
			objs: []client.Object{
				orgRole("ceo", "", false),
				orgRole("pm", "ceo", false, ps("prd", "ghost")),
				orgAgent("c", "ceo"), orgAgent("p", "pm"),
			},
			team: []string{"c", "p"},
			want: "not part of this team's composition",
		},
		{
			name: "process-delegation-cycle",
			objs: []client.Object{
				orgRole("ceo", "", false),
				orgRole("pm", "ceo", false, ps("build", "eng")),
				orgRole("eng", "pm", false, ps("escalate", "pm")),
				orgAgent("c", "ceo"), orgAgent("p", "pm"), orgAgent("e", "eng"),
			},
			team: []string{"c", "p", "e"},
			want: "delegation is cyclic",
		},
		{
			name: "process-self-loop",
			objs: []client.Object{
				orgRole("ceo", "", false),
				orgRole("pm", "ceo", false, ps("signoff", "pm")),
				orgAgent("c", "ceo"), orgAgent("p", "pm"),
			},
			team: []string{"c", "p"},
			want: "delegation is cyclic",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := orgValidator(t, tc.objs...)
			err := toInvalid("Team", "squad-alpha", v.ValidateTeam(ctx, orgTeam(tc.team...)))
			require.Error(t, err, tc.name)
			assert.Contains(t, err.Error(), tc.want, tc.name)
		})
	}
}

// TestTeamOrgChartGuardIsLoadBearing is the WS-A falsification case:
// disabling GuardTeamOrgChart must flip the dangling-manager denial to
// admit — the guard owns the invariant, no other guard double-covers it.
func TestTeamOrgChartGuardIsLoadBearing(t *testing.T) {
	ctx := context.Background()
	objs := []client.Object{orgRole("a", "ghost", false), orgRole("ceo", "", false), orgAgent("x", "a"), orgAgent("c", "ceo")}
	v := orgValidator(t, objs...)
	v.DisabledGuards = map[string]bool{GuardTeamOrgChart: true}
	assert.NoError(t, toInvalid("Team", "squad-alpha", v.ValidateTeam(ctx, orgTeam("x", "c"))),
		"guard removed but its case still rejects — another guard double-covers it")
}
