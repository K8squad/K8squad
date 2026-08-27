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

package networkpolicy

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// These tests are DB-free: they exercise the pure NetworkPolicy builders and the
// client-backed Ensure/Reconcile/Delete flows against the controller-runtime fake
// client (an in-memory object tracker). No Postgres — they run in the ci.yml unit
// lane and lift the gated coverage number (ISI-3213).

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ksquadv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add ksquad scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(s); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	return s
}

func newTeam() *ksquadv1alpha1.Team {
	return &ksquadv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alpha",
			Namespace: "team-alpha",
			UID:       types.UID("uid-alpha"),
		},
	}
}

func newManager(t *testing.T, objs ...runtime.Object) *Manager {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(testScheme(t))
	for _, o := range objs {
		b = b.WithRuntimeObjects(o)
	}
	return NewNetworkPolicyManager(b.Build())
}

func mustGetPolicy(t *testing.T, m *Manager, name, ns string) *networkingv1.NetworkPolicy {
	t.Helper()
	np := &networkingv1.NetworkPolicy{}
	if err := m.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, np); err != nil {
		t.Fatalf("get policy %s: %v", name, err)
	}
	return np
}

func TestReconcileCreatesAllThreePolicies(t *testing.T) {
	team := newTeam()
	m := newManager(t, team)

	res, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: team.Name, Namespace: team.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue result: %+v", res)
	}

	for _, name := range []string{
		PolicyTeamIsolation + "-alpha",
		PolicyTeamEgress + "-alpha",
		PolicyTeamIngress + "-alpha",
	} {
		np := mustGetPolicy(t, m, name, team.Namespace)
		if np.Labels[LabelTeam] != team.Name {
			t.Errorf("%s: team label = %q, want %q", name, np.Labels[LabelTeam], team.Name)
		}
		if len(np.OwnerReferences) != 1 || np.OwnerReferences[0].UID != team.UID {
			t.Errorf("%s: missing/incorrect owner reference: %+v", name, np.OwnerReferences)
		}
		if np.OwnerReferences[0].Kind != "Team" {
			t.Errorf("%s: owner kind = %q, want Team", name, np.OwnerReferences[0].Kind)
		}
		if got := np.OwnerReferences[0].Controller; got == nil || !*got {
			t.Errorf("%s: owner Controller should be true", name)
		}
	}
}

func TestReconcileMissingTeamIsNoOp(t *testing.T) {
	m := newManager(t) // no Team object present
	res, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ghost", Namespace: "team-ghost"},
	})
	if err != nil {
		t.Fatalf("reconcile of absent team should be a no-op, got: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestReconcileDeletingTeamIsNoOp(t *testing.T) {
	team := newTeam()
	now := metav1.Now()
	team.DeletionTimestamp = &now
	team.Finalizers = []string{"k8squad.io/test"} // required for a valid deletion-timestamped object
	m := newManager(t, team)

	if _, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: team.Name, Namespace: team.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// No policies should have been created for a deleting team.
	np := &networkingv1.NetworkPolicy{}
	err := m.client.Get(context.Background(), types.NamespacedName{Name: PolicyTeamIsolation + "-alpha", Namespace: team.Namespace}, np)
	if !errors.IsNotFound(err) {
		t.Errorf("expected no isolation policy for deleting team, got err=%v", err)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	team := newTeam()
	m := newManager(t, team)
	ctx := context.Background()

	if err := m.EnsureTeamIsolation(ctx, team); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	first := mustGetPolicy(t, m, PolicyTeamIsolation+"-alpha", team.Namespace)
	rv := first.ResourceVersion

	// Second ensure with an unchanged spec must not issue an update (ResourceVersion stable).
	if err := m.EnsureTeamIsolation(ctx, team); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	second := mustGetPolicy(t, m, PolicyTeamIsolation+"-alpha", team.Namespace)
	if second.ResourceVersion != rv {
		t.Errorf("idempotent ensure changed ResourceVersion %s -> %s", rv, second.ResourceVersion)
	}
}

func TestEnsureUpdatesDriftedSpec(t *testing.T) {
	team := newTeam()
	m := newManager(t, team)
	ctx := context.Background()

	if err := m.EnsureTeamEgress(ctx, team); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Mutate the live policy so the desired spec differs, forcing the update branch.
	drift := mustGetPolicy(t, m, PolicyTeamEgress+"-alpha", team.Namespace)
	drift.Spec.Egress = nil
	if err := m.client.Update(ctx, drift); err != nil {
		t.Fatalf("drift update: %v", err)
	}

	if err := m.EnsureTeamEgress(ctx, team); err != nil {
		t.Fatalf("reconciling ensure: %v", err)
	}
	healed := mustGetPolicy(t, m, PolicyTeamEgress+"-alpha", team.Namespace)
	if len(healed.Spec.Egress) == 0 {
		t.Errorf("expected drifted egress spec to be reconciled back to desired, got empty")
	}
}

func TestEnsureAllThreeSurfaces(t *testing.T) {
	team := newTeam()
	m := newManager(t, team)
	ctx := context.Background()

	if err := m.EnsureTeamIsolation(ctx, team); err != nil {
		t.Fatalf("isolation: %v", err)
	}
	if err := m.EnsureTeamIngress(ctx, team); err != nil {
		t.Fatalf("ingress: %v", err)
	}
	if err := m.EnsureTeamEgress(ctx, team); err != nil {
		t.Fatalf("egress: %v", err)
	}

	iso := mustGetPolicy(t, m, PolicyTeamIsolation+"-alpha", team.Namespace)
	if len(iso.Spec.PolicyTypes) != 2 {
		t.Errorf("isolation policy should carry both Ingress+Egress policy types, got %v", iso.Spec.PolicyTypes)
	}
	ing := mustGetPolicy(t, m, PolicyTeamIngress+"-alpha", team.Namespace)
	if len(ing.Spec.Ingress) == 0 {
		t.Errorf("ingress policy has no ingress rules")
	}
	eg := mustGetPolicy(t, m, PolicyTeamEgress+"-alpha", team.Namespace)
	if len(eg.Spec.Egress) == 0 {
		t.Errorf("egress policy has no egress rules")
	}
}

func TestDeleteTeamPolicies(t *testing.T) {
	team := newTeam()
	m := newManager(t, team)
	ctx := context.Background()

	// Create all three, then delete them.
	if _, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: team.Name, Namespace: team.Namespace}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := m.DeleteTeamPolicies(ctx, team); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, name := range []string{
		PolicyTeamIsolation + "-alpha",
		PolicyTeamEgress + "-alpha",
		PolicyTeamIngress + "-alpha",
	} {
		np := &networkingv1.NetworkPolicy{}
		err := m.client.Get(ctx, types.NamespacedName{Name: name, Namespace: team.Namespace}, np)
		if !errors.IsNotFound(err) {
			t.Errorf("%s should be deleted, got err=%v", name, err)
		}
	}
}

func TestDeleteTeamPoliciesMissingIsNoOp(t *testing.T) {
	team := newTeam()
	m := newManager(t, team) // no policies created
	if err := m.DeleteTeamPolicies(context.Background(), team); err != nil {
		t.Errorf("deleting absent policies should be a no-op, got: %v", err)
	}
}

func TestPolicyBuildersDeterministic(t *testing.T) {
	team := newTeam()
	m := newManager(t, team)

	iso1 := m.createTeamIsolationPolicy(team)
	iso2 := m.createTeamIsolationPolicy(team)
	if iso1.Name != iso2.Name || iso1.Namespace != team.Namespace {
		t.Errorf("isolation builder not deterministic / wrong namespace: %+v vs %+v", iso1.ObjectMeta, iso2.ObjectMeta)
	}
	if iso1.Name != PolicyTeamIsolation+"-"+team.Name {
		t.Errorf("isolation policy name = %q", iso1.Name)
	}

	eg := m.createTeamEgressPolicy(team)
	if eg.Name != PolicyTeamEgress+"-"+team.Name {
		t.Errorf("egress policy name = %q", eg.Name)
	}
	ing := m.createTeamIngressPolicy(team)
	if ing.Name != PolicyTeamIngress+"-"+team.Name {
		t.Errorf("ingress policy name = %q", ing.Name)
	}
}

func TestEnsureCreateErrorsPropagate(t *testing.T) {
	team := newTeam()
	boom := errors.NewInternalError(context.DeadlineExceeded)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRuntimeObjects(team).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return boom
			},
		}).Build()
	m := NewNetworkPolicyManager(c)
	ctx := context.Background()

	if err := m.EnsureTeamIsolation(ctx, team); err == nil {
		t.Error("isolation create error should propagate")
	}
	if err := m.EnsureTeamEgress(ctx, team); err == nil {
		t.Error("egress create error should propagate")
	}
	if err := m.EnsureTeamIngress(ctx, team); err == nil {
		t.Error("ingress create error should propagate")
	}
	// Reconcile bubbles the first Ensure error up.
	if _, err := m.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: team.Name, Namespace: team.Namespace}}); err == nil {
		t.Error("reconcile should surface the create error")
	}
}

func TestEnsureGetErrorPropagates(t *testing.T) {
	team := newTeam()
	boom := errors.NewServiceUnavailable("apiserver down")
	// A non-NotFound Get error on the policy lookup must fail (not be swallowed).
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRuntimeObjects(team).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				// Let the Team fetch (in Reconcile) succeed; only fail NetworkPolicy Gets.
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	m := NewNetworkPolicyManager(c)
	if err := m.EnsureTeamIsolation(context.Background(), team); err == nil {
		t.Error("non-NotFound get error should propagate")
	}
}

func TestDeleteGetErrorPropagates(t *testing.T) {
	team := newTeam()
	boom := errors.NewServiceUnavailable("apiserver down")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRuntimeObjects(team).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	m := NewNetworkPolicyManager(c)
	if err := m.DeleteTeamPolicies(context.Background(), team); err == nil {
		t.Error("delete get error (non-NotFound) should propagate")
	}
}

func TestPtrTo(t *testing.T) {
	b := ptrTo(true)
	if b == nil || *b != true {
		t.Fatalf("ptrTo(true) = %v", b)
	}
	i := ptrTo(42)
	if i == nil || *i != 42 {
		t.Fatalf("ptrTo(42) = %v", i)
	}
}
