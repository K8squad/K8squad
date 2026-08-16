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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// TestAttributionOnRealCRDs proves the story 1.6 contract against the real
// squad-composition types (not fakes): the webhook stamps attribution from
// the admission principal, rejects forged/created-by mutations, and the
// RBAC scope query via the ownedBy cache index returns exactly the
// resources the principal owns.
func TestAttributionOnRealCRDs(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme setup: %v", err)
	}

	newTeam := func(name string) *ksquadv1.Team {
		return &ksquadv1.Team{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ksquad.io/v1alpha1", Kind: "Team"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "squad-a"},
			Spec:       ksquadv1.TeamSpec{NamespaceStrategy: "dedicated"},
		}
	}
	newRun := func(name string) *ksquadv1.Run {
		return &ksquadv1.Run{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ksquad.io/v1alpha1", Kind: "Run"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "squad-a"},
			Spec:       ksquadv1.RunSpec{TeamRef: ksquadv1.ObjectRef{Name: "alpha"}, WorkItemRef: "wi-123"},
		}
	}

	w := &AttributionWebhook{}
	admissionCtx := ctxWithUser("henrik")

	// Admission path for a real Team: default then validate.
	team := newTeam("alpha")
	if err := w.Default(admissionCtx, team); err != nil {
		t.Fatalf("Default(Team) error = %v", err)
	}
	if _, err := w.ValidateCreate(admissionCtx, team); err != nil {
		t.Fatalf("ValidateCreate(Team) error = %v", err)
	}
	createdBy, ok := ksquadv1.GetCreatedBy(team)
	if !ok || createdBy != "henrik" {
		t.Fatalf("Team created-by = %q (present=%v), want henrik", createdBy, ok)
	}
	if team.Spec.OwnedBy != "henrik" {
		t.Fatalf("Team ownedBy = %q, want henrik", team.Spec.OwnedBy)
	}

	// Forged created-by on a real Run is rejected at create.
	forgedRun := newRun("forged")
	ksquadv1.SetCreatedBy(forgedRun, "alice")
	if _, err := w.ValidateCreate(ctxWithUser("henrik"), forgedRun); err == nil {
		t.Fatal("ValidateCreate(Run with forged created-by) = nil, want rejection")
	}

	// ownedBy transfer on update is legal; created-by drift is not.
	run := newRun("beta")
	if err := w.Default(admissionCtx, run); err != nil {
		t.Fatalf("Default(Run) error = %v", err)
	}
	updated := run.DeepCopy()
	updated.Spec.OwnedBy = "bob"
	if _, err := w.ValidateUpdate(admissionCtx, run, updated); err != nil {
		t.Fatalf("ValidateUpdate(ownedBy transfer) error = %v, want nil", err)
	}
	mutated := run.DeepCopy()
	ksquadv1.SetCreatedBy(mutated, "mallory")
	if _, err := w.ValidateUpdate(admissionCtx, run, mutated); err == nil {
		t.Fatal("ValidateUpdate(created-by mutation) = nil, want rejection")
	}

	// RBAC scope query over the real types via the cache index.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, updated).
		WithIndex(&ksquadv1.Team{}, ksquadv1.OwnedByIndexKey, ksquadv1.OwnedByIndexValue).
		WithIndex(&ksquadv1.Run{}, ksquadv1.OwnedByIndexKey, ksquadv1.OwnedByIndexValue).
		Build()

	var henrikTeams ksquadv1.TeamList
	if err := c.List(ctx, &henrikTeams, client.MatchingFields{ksquadv1.OwnedByIndexKey: "henrik"}); err != nil {
		t.Fatalf("List Teams owned by henrik: %v", err)
	}
	if len(henrikTeams.Items) != 1 || henrikTeams.Items[0].Name != "alpha" {
		t.Fatalf("Teams owned by henrik = %d items, want exactly [alpha]", len(henrikTeams.Items))
	}

	var bobRuns ksquadv1.RunList
	if err := c.List(ctx, &bobRuns, client.MatchingFields{ksquadv1.OwnedByIndexKey: "bob"}); err != nil {
		t.Fatalf("List Runs owned by bob: %v", err)
	}
	if len(bobRuns.Items) != 1 || bobRuns.Items[0].Name != "beta" {
		t.Fatalf("Runs owned by bob = %d items, want exactly [beta]", len(bobRuns.Items))
	}

	// The webhook-stamped annotation round-trips through the object key the
	// apiserver would use.
	var fetched ksquadv1.Team
	if err := c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: "alpha"}, &fetched); err != nil {
		t.Fatalf("Get Team: %v", err)
	}
	if p, _ := ksquadv1.GetCreatedBy(&fetched); p != "henrik" {
		t.Fatalf("persisted Team created-by = %q, want henrik", p)
	}
}
