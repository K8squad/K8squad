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
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

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

// --- ISI-4779: EligibleReviewers (the D5 roster the dropdown pre-filters to) ---

func names(refs []ReviewerRef) map[string]bool {
	m := map[string]bool{}
	for _, r := range refs {
		m[r.Name] = true
	}
	return m
}

// EligibleReviewers returns ONLY code_review-capable team agents, skipping the
// non-capable and the dangling refs — the exact set CheckReviewerEligibility
// accepts. This is the single-source-of-truth invariant that keeps the dropdown
// pre-filter and the write-time 422 from ever disagreeing.
func TestEligibleReviewers_FiltersToCapableAndAgreesWithCheck(t *testing.T) {
	r := newReader(t,
		team(
			ksquadv1.ObjectRef{Name: "rev"},      // capable
			ksquadv1.ObjectRef{Name: "writer"},   // role lacks code_review
			ksquadv1.ObjectRef{Name: "ghost"},    // no Agent CR (dangling)
			ksquadv1.ObjectRef{Name: "roleless"}, // agent → missing Role
		),
		agentWithRole("rev", "reviewer-role"),
		agentWithRole("writer", "writer-role"),
		agentWithRole("roleless", "ghost-role"),
		role("reviewer-role", "implementation", "code_review"),
		role("writer-role", "implementation"),
	)
	refs, err := EligibleReviewers(context.Background(), r, teamNS)
	if err != nil {
		t.Fatalf("EligibleReviewers: %v", err)
	}
	got := names(refs)
	if len(refs) != 1 || !got["rev"] {
		t.Fatalf("want exactly [rev], got %+v", refs)
	}
	// Consistency: every name the roster lists passes the write-time check, and
	// every name it omits fails it.
	for _, n := range []string{"rev", "writer", "ghost", "roleless"} {
		accepted := CheckReviewerEligibility(context.Background(), r, teamNS, n) == nil
		if accepted != got[n] {
			t.Fatalf("roster/check disagree for %q: roster=%v check-accepts=%v", n, got[n], accepted)
		}
	}
}

func TestEligibleReviewers_NoTeamIsEmptyNotError(t *testing.T) {
	r := newReader(t) // no Team owns the namespace
	refs, err := EligibleReviewers(context.Background(), r, teamNS)
	if err != nil {
		t.Fatalf("no-team must be an empty roster, not an error: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("want empty roster, got %+v", refs)
	}
}

// --- ISI-5666: the SHIPPED roles must agree with D5 so the reviewer dropdown is
// not empty by construction. This decodes the actual example Role manifests and
// asserts, against the SAME predicate the apiserver dropdown and the write-time
// 422 use, that exactly the dedicated Code Reviewer role is code_review-capable
// and every other role stays non-capable (guarding the TRAP: naively phase-
// binding a non-reviewer role here would narrow its E4 dispatch).
func TestShippedBmadRolesMatchD5Capability(t *testing.T) {
	// Repo-root-relative: pkg/reviewauto -> ../.. -> examples/bmad-team.
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "bmad-team", "05-roles.yaml"))
	if err != nil {
		t.Fatalf("read bmad-team roles manifest: %v", err)
	}

	// The role that MUST be code_review-capable so its agent (amelia, the
	// dedicated Code Reviewer) is surfaced by EligibleReviewers.
	const reviewerRole = "code-reviewer"

	capable := map[string]bool{}
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var role ksquadv1.Role
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			t.Fatalf("decode Role doc: %v\n---\n%s", err, doc)
		}
		if role.Kind != "" && role.Kind != "Role" {
			continue // skip any non-Role document defensively
		}
		if role.Name == "" {
			continue
		}
		capable[role.Name] = RoleCodeReviewCapable(&role)
	}

	if len(capable) == 0 {
		t.Fatal("decoded no Role documents from the manifest")
	}
	if !capable[reviewerRole] {
		t.Fatalf("role %q must declare activePhases:[code_review] so the reviewer dropdown is non-empty (D5)", reviewerRole)
	}
	for name, isCapable := range capable {
		if name != reviewerRole && isCapable {
			t.Fatalf("only %q should be code_review-capable, but %q also is — phase-binding a non-reviewer role narrows its E4 dispatch (ISI-5666 TRAP)", reviewerRole, name)
		}
	}
}
