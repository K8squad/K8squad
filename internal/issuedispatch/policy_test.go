package issuedispatch

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
		Spec: ksquadv1.TeamSpec{
			Agents:            []ksquadv1.ObjectRef{{Name: "triage-bot"}},
			NamespaceStrategy: "managed",
		},
	}
}

func polProject(it *ksquadv1.IssueTriageSpec) *ksquadv1.Project {
	var auto *ksquadv1.RepoAutomationSpec
	if it != nil {
		auto = &ksquadv1.RepoAutomationSpec{IssueTriage: it}
	}
	return &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: polNS, UID: types.UID(polProjUID)},
		Spec: ksquadv1.ProjectSpec{
			Repo: ksquadv1.RepoSpec{
				URL:        "https://github.com/acme/widget",
				Automation: auto,
			},
		},
	}
}

func enabledSpec() *ksquadv1.IssueTriageSpec {
	now := metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	return &ksquadv1.IssueTriageSpec{
		Enabled:       true,
		TriageAgentID: "triage-bot",
		EnabledBy:     "user:alice",
		EnabledAt:     &now,
	}
}

func TestTriagePolicyEnabled(t *testing.T) {
	r := NewProjectPolicyReader(polReader(t, polProject(enabledSpec()), polTeam()))
	pol, err := r.TriagePolicy(context.Background(), polNS, "widget")
	if err != nil {
		t.Fatalf("TriagePolicy: %v", err)
	}
	if pol == nil {
		t.Fatal("want a policy, got nil")
	}
	if !pol.Enabled || pol.TriageAgentID != "triage-bot" {
		t.Errorf("policy core wrong: %+v", pol)
	}
	if pol.ProjectID != polProjUID || pol.TeamID != polTeamUID {
		t.Errorf("coord ids wrong: ProjectID=%q TeamID=%q", pol.ProjectID, pol.TeamID)
	}
	if pol.EnabledBy != "user:alice" || pol.EnabledAt.IsZero() {
		t.Errorf("provenance/watermark wrong: by=%q at=%v", pol.EnabledBy, pol.EnabledAt)
	}
	if !pol.OnlyUnassigned {
		t.Error("OnlyUnassigned should default true")
	}
}

func TestTriagePolicyDisabledOrAbsentIsNil(t *testing.T) {
	disabled := enabledSpec()
	disabled.Enabled = false
	cases := []struct {
		name    string
		objs    []client.Object
		project string
	}{
		{"disabled", []client.Object{polProject(disabled), polTeam()}, "widget"},
		{"no-automation", []client.Object{polProject(nil), polTeam()}, "widget"},
		{"vanished-project", []client.Object{polTeam()}, "ghost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewProjectPolicyReader(polReader(t, tc.objs...))
			pol, err := r.TriagePolicy(context.Background(), polNS, tc.project)
			if err != nil {
				t.Fatalf("TriagePolicy: %v", err)
			}
			if pol != nil {
				t.Fatalf("want nil policy, got %+v", pol)
			}
		})
	}
}

func TestTriagePolicyMissingProvenance(t *testing.T) {
	spec := enabledSpec()
	spec.EnabledBy = ""
	r := NewProjectPolicyReader(polReader(t, polProject(spec), polTeam()))
	_, err := r.TriagePolicy(context.Background(), polNS, "widget")
	if !errors.Is(err, ErrPolicyMissingProvenance) {
		t.Fatalf("want ErrPolicyMissingProvenance, got %v", err)
	}
}

func TestTriagePolicyMissingEnabledAt(t *testing.T) {
	spec := enabledSpec()
	spec.EnabledAt = nil
	r := NewProjectPolicyReader(polReader(t, polProject(spec), polTeam()))
	_, err := r.TriagePolicy(context.Background(), polNS, "widget")
	if !errors.Is(err, ErrPolicyMissingEnabledAt) {
		t.Fatalf("want ErrPolicyMissingEnabledAt, got %v", err)
	}
}

func TestTriagePolicyOnlyUnassignedFalseAndLabels(t *testing.T) {
	spec := enabledSpec()
	no := false
	spec.OnlyUnassigned = &no
	spec.LabelFilter = []string{"bug"}
	r := NewProjectPolicyReader(polReader(t, polProject(spec), polTeam()))
	pol, err := r.TriagePolicy(context.Background(), polNS, "widget")
	if err != nil {
		t.Fatalf("TriagePolicy: %v", err)
	}
	if pol.OnlyUnassigned {
		t.Error("OnlyUnassigned=false should pass through")
	}
	if len(pol.LabelFilter) != 1 || pol.LabelFilter[0] != "bug" {
		t.Errorf("LabelFilter = %v", pol.LabelFilter)
	}
}
