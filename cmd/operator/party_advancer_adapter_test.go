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

package main

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ISI-5669 WS1 (root-cause B) — teamCoordinatorRoster must resolve the REAL Coordinator-role agent
// off the Agent→Role→Coordinator flag, NOT the first team agent. These tests pin that: the advancer
// dispatches the facilitator run to this name, and the mint-gate (party_dispatch.go coordinatorOf)
// only sequences a round when the posting author IS the coordinator — so returning the wrong name
// (the old first-agent degrade) silently falls through to the parallel broadcast ISI-5662 reported.

const partyTestNS = "squad-acme"

// partyRosterClient builds a fake Team-reading client.Reader seeded with one Team (uid + home
// namespace + ordered agent refs) and the matching Agent + Role CRs. coordinatorRoleName is the
// Role whose Spec.Coordinator is true ("" ⇒ no coordinator role exists). agentRoles maps each
// agent name (in roster order) to the role it references.
func partyRosterClient(t *testing.T, teamUID string, agentOrder []string, agentRoles map[string]string, coordinatorRoleName string) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add ksquad scheme: %v", err)
	}

	objs := []client.Object{
		&ksquadv1.Team{
			ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: partyTestNS, UID: types.UID(teamUID)},
			Spec:       teamSpecWithAgents(agentOrder),
		},
	}
	// De-dupe the role set across agents; stamp Coordinator on the named role.
	roleSeen := map[string]bool{}
	for _, name := range agentOrder {
		role := agentRoles[name]
		objs = append(objs, &ksquadv1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: partyTestNS},
			Spec:       ksquadv1.AgentSpec{RoleRef: ksquadv1.ObjectRef{Name: role}},
		})
		if role != "" && !roleSeen[role] {
			roleSeen[role] = true
			objs = append(objs, &ksquadv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: role, Namespace: partyTestNS},
				Spec:       ksquadv1.RoleSpec{Coordinator: role == coordinatorRoleName},
			})
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func teamSpecWithAgents(names []string) ksquadv1.TeamSpec {
	refs := make([]ksquadv1.ObjectRef, 0, len(names))
	for _, n := range names {
		refs = append(refs, ksquadv1.ObjectRef{Name: n})
	}
	return ksquadv1.TeamSpec{NamespaceStrategy: "existing", Agents: refs}
}

func TestCoordinatorForTeam_ResolvesRealCoordinatorNotFirstAgent(t *testing.T) {
	// "worker" is the first roster agent but a plain coder; "lead" carries the Coordinator role and
	// must win even though it is second in the roster.
	reader := partyRosterClient(t, "team-uid-1",
		[]string{"worker", "lead", "helper"},
		map[string]string{"worker": "coder", "lead": "coordinator-role", "helper": "coder"},
		"coordinator-role",
	)
	r := teamCoordinatorRoster{reader: reader}
	name, ok, err := r.CoordinatorForTeam(context.Background(), "team-uid-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected a coordinator to resolve")
	}
	if name != "lead" {
		t.Fatalf("resolved coordinator = %q, want %q (the real Coordinator-role agent, not the first agent)", name, "lead")
	}
}

func TestCoordinatorForTeam_NoCoordinatorRoleFailsSafe(t *testing.T) {
	// A room with no Coordinator-role agent must return ok=false with a nil error (fail safe) so the
	// advancer declines to mint — never a degrade to the first agent, which would broadcast.
	reader := partyRosterClient(t, "team-uid-2",
		[]string{"worker", "helper"},
		map[string]string{"worker": "coder", "helper": "coder"},
		"", // no coordinator role
	)
	r := teamCoordinatorRoster{reader: reader}
	name, ok, err := r.CoordinatorForTeam(context.Background(), "team-uid-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || name != "" {
		t.Fatalf("no Coordinator-role agent must fail safe: got (name=%q, ok=%v), want (\"\", false)", name, ok)
	}
}

func TestCoordinatorForTeam_DanglingTeamUIDErrors(t *testing.T) {
	reader := partyRosterClient(t, "team-uid-3",
		[]string{"lead"},
		map[string]string{"lead": "coordinator-role"},
		"coordinator-role",
	)
	r := teamCoordinatorRoster{reader: reader}
	if _, ok, err := r.CoordinatorForTeam(context.Background(), "no-such-uid"); err == nil || ok {
		t.Fatalf("a team uid resolving to no Team CR must error, got (ok=%v, err=%v)", ok, err)
	}
}

func TestCoordinatorForTeam_Guards(t *testing.T) {
	var r teamCoordinatorRoster // nil reader
	if _, _, err := r.CoordinatorForTeam(context.Background(), "x"); err == nil {
		t.Fatalf("nil reader must error")
	}
	reader := partyRosterClient(t, "team-uid-4", []string{"lead"},
		map[string]string{"lead": "coordinator-role"}, "coordinator-role")
	r = teamCoordinatorRoster{reader: reader}
	if _, _, err := r.CoordinatorForTeam(context.Background(), "  "); err == nil {
		t.Fatalf("empty team uid must error")
	}
}
