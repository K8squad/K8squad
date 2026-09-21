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

package reviewauto

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

const teamNS = "squad-alpha"

func newReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func team(agents ...ksquadv1.ObjectRef) *ksquadv1.Team {
	return &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: teamNS},
		Spec:       ksquadv1.TeamSpec{Agents: agents, NamespaceStrategy: "managed"},
	}
}

func agentWithRole(name, roleName string) *ksquadv1.Agent {
	return &ksquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: teamNS},
		Spec: ksquadv1.AgentSpec{
			RuntimeRef: ksquadv1.ObjectRef{Name: "rt"},
			RoleRef:    ksquadv1.ObjectRef{Name: roleName},
		},
	}
}

func role(name string, phases ...string) *ksquadv1.Role {
	return &ksquadv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: teamNS},
		Spec:       ksquadv1.RoleSpec{ActivePhases: phases},
	}
}

func TestRoleCodeReviewCapable(t *testing.T) {
	cases := []struct {
		name string
		role *ksquadv1.Role
		want bool
	}{
		{"nil role", nil, false},
		{"empty phases (phase-agnostic) is NOT auto-capable", role("r"), false},
		{"has code_review", role("r", "implementation", "code_review"), true},
		{"other phases only", role("r", "design", "testing"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoleCodeReviewCapable(tc.role); got != tc.want {
				t.Fatalf("RoleCodeReviewCapable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckReviewerEligibility_EmptyAgentID(t *testing.T) {
	r := newReader(t)
	if err := CheckReviewerEligibility(context.Background(), r, teamNS, ""); !errors.Is(err, ErrReviewerAgentRequired) {
		t.Fatalf("want ErrReviewerAgentRequired, got %v", err)
	}
}

func TestCheckReviewerEligibility_Eligible(t *testing.T) {
	r := newReader(t,
		team(ksquadv1.ObjectRef{Name: "rev"}),
		agentWithRole("rev", "reviewer-role"),
		role("reviewer-role", "code_review"),
	)
	if err := CheckReviewerEligibility(context.Background(), r, teamNS, "rev"); err != nil {
		t.Fatalf("want eligible (nil), got %v", err)
	}
}

func TestCheckReviewerEligibility_NotTeamAgent(t *testing.T) {
	// Team composition does not list "rev".
	r := newReader(t,
		team(ksquadv1.ObjectRef{Name: "someone-else"}),
		agentWithRole("rev", "reviewer-role"),
		role("reviewer-role", "code_review"),
	)
	if err := CheckReviewerEligibility(context.Background(), r, teamNS, "rev"); !errors.Is(err, ErrReviewerNotTeamAgent) {
		t.Fatalf("want ErrReviewerNotTeamAgent, got %v", err)
	}
}

func TestCheckReviewerEligibility_NoTeamForNamespace(t *testing.T) {
	r := newReader(t, agentWithRole("rev", "reviewer-role"), role("reviewer-role", "code_review"))
	if err := CheckReviewerEligibility(context.Background(), r, teamNS, "rev"); !errors.Is(err, ErrReviewerNotTeamAgent) {
		t.Fatalf("want ErrReviewerNotTeamAgent (no team owns ns), got %v", err)
	}
}

func TestCheckReviewerEligibility_MissingAgentCR(t *testing.T) {
	// Composition names "rev" but no Agent CR exists.
	r := newReader(t, team(ksquadv1.ObjectRef{Name: "rev"}))
	if err := CheckReviewerEligibility(context.Background(), r, teamNS, "rev"); !errors.Is(err, ErrReviewerNotTeamAgent) {
		t.Fatalf("want ErrReviewerNotTeamAgent (dangling agent ref), got %v", err)
	}
}

func TestCheckReviewerEligibility_NoCapability(t *testing.T) {
	r := newReader(t,
		team(ksquadv1.ObjectRef{Name: "rev"}),
		agentWithRole("rev", "writer-role"),
		role("writer-role", "implementation"),
	)
	if err := CheckReviewerEligibility(context.Background(), r, teamNS, "rev"); !errors.Is(err, ErrReviewerNoCapability) {
		t.Fatalf("want ErrReviewerNoCapability, got %v", err)
	}
}

func TestCheckReviewerEligibility_DanglingRoleRef(t *testing.T) {
	// Agent references a Role CR that does not exist ⇒ treated as no-capability.
	r := newReader(t,
		team(ksquadv1.ObjectRef{Name: "rev"}),
		agentWithRole("rev", "ghost-role"),
	)
	if err := CheckReviewerEligibility(context.Background(), r, teamNS, "rev"); !errors.Is(err, ErrReviewerNoCapability) {
		t.Fatalf("want ErrReviewerNoCapability (dangling role ref), got %v", err)
	}
}
