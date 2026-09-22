package reviewdispatch

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reviewauto"
)

const (
	polNS      = "squad-alpha"
	polTeamUID = "team-uid-1"
	polProjUID = "proj-uid-1"
)

func polReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func polTeam() *ksquadv1.Team {
	return &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name: "alpha", Namespace: polNS, UID: types.UID(polTeamUID),
		},
		Spec: ksquadv1.TeamSpec{
			Agents:            []ksquadv1.ObjectRef{{Name: "reviewer-bot"}},
			NamespaceStrategy: "managed",
		},
	}
}

func polAgentAndRole(capable bool) []client.Object {
	phases := []string{"implementation"}
	if capable {
		phases = []string{"implementation", "code_review"}
	}
	return []client.Object{
		&ksquadv1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "reviewer-bot", Namespace: polNS},
			Spec: ksquadv1.AgentSpec{
				RuntimeRef: ksquadv1.ObjectRef{Name: "rt"},
				RoleRef:    ksquadv1.ObjectRef{Name: "cr-role"},
			},
		},
		&ksquadv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "cr-role", Namespace: polNS},
			Spec:       ksquadv1.RoleSpec{ActivePhases: phases},
		},
	}
}

func polProject(ra *ksquadv1.ReviewAutomationSpec) *ksquadv1.Project {
	return &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: "widget", Namespace: polNS, UID: types.UID(polProjUID),
		},
		Spec: ksquadv1.ProjectSpec{
			Repo: ksquadv1.RepoSpec{
				URL:              "https://github.com/acme/widget",
				ReviewAutomation: ra,
			},
		},
	}
}

// TestReviewPolicyEnabledEligible: the happy path maps the CR spec + coord ids onto
// a Policy, threading the server-stamped EnabledBy provenance.
func TestReviewPolicyEnabledEligible(t *testing.T) {
	objs := []client.Object{
		polProject(&ksquadv1.ReviewAutomationSpec{
			Enabled:         true,
			ReviewerAgentID: "reviewer-bot",
			Scope:           "team_authored",
			Trigger:         "on_new_commits",
			EnabledBy:       "user:alice",
		}),
		polTeam(),
	}
	objs = append(objs, polAgentAndRole(true)...)
	r := NewProjectPolicyReader(polReader(t, objs...))

	pol, err := r.ReviewPolicy(context.Background(), polNS, "widget")
	if err != nil {
		t.Fatalf("ReviewPolicy: %v", err)
	}
	if pol == nil {
		t.Fatal("want a policy, got nil")
	}
	if !pol.Enabled || pol.ReviewerAgentID != "reviewer-bot" {
		t.Errorf("policy core wrong: %+v", pol)
	}
	if pol.ProjectID != polProjUID || pol.TeamID != polTeamUID {
		t.Errorf("coord ids wrong: ProjectID=%q TeamID=%q", pol.ProjectID, pol.TeamID)
	}
	if pol.EnabledBy != "user:alice" {
		t.Errorf("EnabledBy provenance = %q, want user:alice", pol.EnabledBy)
	}
}

// TestReviewPolicyDisabledOrAbsentIsNil: disabled, nil spec, and a vanished project
// all resolve to (nil, nil) — a no-op pass.
func TestReviewPolicyDisabledOrAbsentIsNil(t *testing.T) {
	cases := []struct {
		name    string
		objs    []client.Object
		project string
	}{
		{"disabled", []client.Object{polProject(&ksquadv1.ReviewAutomationSpec{Enabled: false}), polTeam()}, "widget"},
		{"nil-spec", []client.Object{polProject(nil), polTeam()}, "widget"},
		{"project-vanished", []client.Object{polTeam()}, "ghost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewProjectPolicyReader(polReader(t, tc.objs...))
			pol, err := r.ReviewPolicy(context.Background(), polNS, tc.project)
			if err != nil {
				t.Fatalf("ReviewPolicy: %v", err)
			}
			if pol != nil {
				t.Fatalf("want nil policy, got %+v", pol)
			}
		})
	}
}

// TestReviewPolicyMissingProvenance: an enabled policy with no server-stamped
// EnabledBy fails closed — never dispatched unprovenanced.
func TestReviewPolicyMissingProvenance(t *testing.T) {
	objs := []client.Object{
		polProject(&ksquadv1.ReviewAutomationSpec{
			Enabled: true, ReviewerAgentID: "reviewer-bot", EnabledBy: "",
		}),
		polTeam(),
	}
	objs = append(objs, polAgentAndRole(true)...)
	r := NewProjectPolicyReader(polReader(t, objs...))
	if _, err := r.ReviewPolicy(context.Background(), polNS, "widget"); !errors.Is(err, ErrPolicyMissingProvenance) {
		t.Fatalf("want ErrPolicyMissingProvenance, got %v", err)
	}
}

// TestReviewPolicyIneligibleReviewerFails: an enabled policy whose reviewer lost the
// code_review capability fails the pass (defensive D5 re-check), never silently
// dispatches to an ineligible reviewer.
func TestReviewPolicyIneligibleReviewerFails(t *testing.T) {
	objs := []client.Object{
		polProject(&ksquadv1.ReviewAutomationSpec{
			Enabled: true, ReviewerAgentID: "reviewer-bot", EnabledBy: "user:alice",
		}),
		polTeam(),
	}
	objs = append(objs, polAgentAndRole(false)...) // role NOT code_review capable
	r := NewProjectPolicyReader(polReader(t, objs...))
	_, err := r.ReviewPolicy(context.Background(), polNS, "widget")
	if !errors.Is(err, reviewauto.ErrReviewerNoCapability) {
		t.Fatalf("want ErrReviewerNoCapability surfaced, got %v", err)
	}
}
