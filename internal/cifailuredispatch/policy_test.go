package cifailuredispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
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
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: polNS, UID: types.UID(polTeamUID)},
		Spec:       ksquadv1.TeamSpec{NamespaceStrategy: "managed"},
	}
}

func polProject(cf *ksquadv1.CiFailureSpec) *ksquadv1.Project {
	return &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: polNS, UID: types.UID(polProjUID)},
		Spec: ksquadv1.ProjectSpec{
			Repo: ksquadv1.RepoSpec{
				URL:        "https://github.com/acme/widget",
				Automation: &ksquadv1.RepoAutomationSpec{CiFailure: cf},
			},
		},
	}
}

var polEnabledAt = metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))

func enabledSpec() *ksquadv1.CiFailureSpec {
	return &ksquadv1.CiFailureSpec{
		Enabled:   true,
		AgentID:   "triage-bot",
		EnabledBy: "user:henrik",
		EnabledAt: &polEnabledAt,
	}
}

func TestPolicyUnconfiguredOrDisabledIsNil(t *testing.T) {
	cases := map[string]*ksquadv1.CiFailureSpec{
		"nil":      nil,
		"disabled": {Enabled: false, AgentID: "x", EnabledBy: "u", EnabledAt: &polEnabledAt},
	}
	for name, cf := range cases {
		r := NewProjectPolicyReader(polReader(t, polProject(cf), polTeam()))
		pol, err := r.CIFailurePolicy(context.Background(), polNS, "widget")
		if err != nil {
			t.Fatalf("%s: unexpected err: %v", name, err)
		}
		if pol != nil {
			t.Fatalf("%s: expected nil policy, got %+v", name, pol)
		}
	}
}

func TestPolicyMissingProjectIsNil(t *testing.T) {
	r := NewProjectPolicyReader(polReader(t, polTeam()))
	pol, err := r.CIFailurePolicy(context.Background(), polNS, "widget")
	if err != nil || pol != nil {
		t.Fatalf("missing project ⇒ (nil,nil), got (%+v, %v)", pol, err)
	}
}

func TestPolicyEnabledMapsAllFields(t *testing.T) {
	spec := enabledSpec()
	spec.Conclusions = []string{"failure", "timed_out"}
	spec.BranchFilter = []string{"main"}
	r := NewProjectPolicyReader(polReader(t, polProject(spec), polTeam()))

	pol, err := r.CIFailurePolicy(context.Background(), polNS, "widget")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pol == nil {
		t.Fatal("expected a resolved policy")
	}
	if !pol.Enabled || pol.AgentID != "triage-bot" || pol.EnabledBy != "user:henrik" {
		t.Fatalf("basic fields wrong: %+v", pol)
	}
	if pol.ProjectID != polProjUID || pol.TeamID != polTeamUID {
		t.Fatalf("coord scope ids wrong: project=%q team=%q", pol.ProjectID, pol.TeamID)
	}
	if !pol.EnabledAt.Equal(polEnabledAt.Time) {
		t.Fatalf("watermark not threaded: %v", pol.EnabledAt)
	}
	if len(pol.Conclusions) != 2 || pol.Conclusions[0] != "failure" {
		t.Fatalf("conclusions not threaded: %+v", pol.Conclusions)
	}
	if len(pol.BranchFilter) != 1 || pol.BranchFilter[0] != "main" {
		t.Fatalf("branch filter not threaded: %+v", pol.BranchFilter)
	}
}

// Empty conclusions resolve to the ["failure"] default via EffectiveConclusions.
func TestPolicyDefaultsConclusions(t *testing.T) {
	r := NewProjectPolicyReader(polReader(t, polProject(enabledSpec()), polTeam()))
	pol, err := r.CIFailurePolicy(context.Background(), polNS, "widget")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(pol.Conclusions) != 1 || pol.Conclusions[0] != "failure" {
		t.Fatalf("expected default [failure], got %+v", pol.Conclusions)
	}
}

func TestPolicyMissingProvenanceFails(t *testing.T) {
	spec := enabledSpec()
	spec.EnabledBy = ""
	r := NewProjectPolicyReader(polReader(t, polProject(spec), polTeam()))
	_, err := r.CIFailurePolicy(context.Background(), polNS, "widget")
	if !errors.Is(err, ErrPolicyMissingProvenance) {
		t.Fatalf("expected ErrPolicyMissingProvenance, got %v", err)
	}
}

func TestPolicyMissingWatermarkFails(t *testing.T) {
	spec := enabledSpec()
	spec.EnabledAt = nil
	r := NewProjectPolicyReader(polReader(t, polProject(spec), polTeam()))
	_, err := r.CIFailurePolicy(context.Background(), polNS, "widget")
	if !errors.Is(err, ErrPolicyMissingWatermark) {
		t.Fatalf("expected ErrPolicyMissingWatermark, got %v", err)
	}
}

// No Team owning the namespace ⇒ unscoped team id (dev host), not an error.
func TestPolicyNoTeamUnscoped(t *testing.T) {
	r := NewProjectPolicyReader(polReader(t, polProject(enabledSpec())))
	pol, err := r.CIFailurePolicy(context.Background(), polNS, "widget")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pol.TeamID != "" {
		t.Fatalf("expected unscoped team id, got %q", pol.TeamID)
	}
}
