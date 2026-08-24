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

package team

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("add ksquad scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := rbacv1.AddToScheme(s); err != nil {
		t.Fatalf("add rbac scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(s); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	return s
}

func fixedClock() metav1.Time { return metav1.Time{Time: metav1.Now().Time} }

func newReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(objs...).WithStatusSubresource(&api.Team{}).Build()
	return &Reconciler{Client: c, Now: fixedClock}, c
}

func reconcileTeam(t *testing.T, r *Reconciler, name string) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	})
	return err
}

// TestProvisionScaffold (AC1-AC5): one reconcile provisions the namespace and
// the full least-privilege scaffold; nothing lands in ksquad-system.
func TestProvisionScaffold(t *testing.T) {
	r, c := newReconciler(t, newTeam("alpha", "uid-alpha"))

	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var team api.Team
	if err := c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &team); err != nil {
		t.Fatalf("get team: %v", err)
	}
	ns := team.Status.Namespace
	if ns == "" || !strings.HasPrefix(ns, namespacePrefix) {
		t.Fatalf("status.namespace = %q, want a %s-prefixed derivation", ns, namespacePrefix)
	}

	// Namespace exists, labelled, not terminating, and NOT ksquad-system (AC7).
	var namespace corev1.Namespace
	if err := c.Get(context.Background(), types.NamespacedName{Name: ns}, &namespace); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if namespace.Labels[LabelTeam] != "alpha" || namespace.Labels[LabelTenancy] != TenancySquad {
		t.Errorf("namespace labels = %v, want team=alpha tenancy=squad", namespace.Labels)
	}
	if IsReservedNamespace(ns) {
		t.Errorf("provisioned into reserved namespace %q (AC7)", ns)
	}

	// SA exists and never automounts the API token.
	var sa corev1.ServiceAccount
	if err := c.Get(context.Background(), types.NamespacedName{Name: AgentServiceAccount, Namespace: ns}, &sa); err != nil {
		t.Fatalf("get SA: %v", err)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Errorf("SA automountServiceAccountToken must be false")
	}

	// Role is namespaced, least-privilege, and grants NO Secret access at all
	// (AC2: a namespace-wide secrets get/list lets any Run read every
	// principal's BYO Secret — the exact hole this closes).
	var role rbacv1.Role
	if err := c.Get(context.Background(), types.NamespacedName{Name: AgentServiceAccount, Namespace: ns}, &role); err != nil {
		t.Fatalf("get Role: %v", err)
	}
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			if res == "*" {
				t.Errorf("wildcard resource in agent Role (AC2)")
			}
			if res == "secrets" {
				t.Errorf("agent Role grants secrets access (AC2 violation: %v)", rule)
			}
		}
		for _, verb := range rule.Verbs {
			if verb == "*" {
				t.Errorf("wildcard verb in agent Role (AC2)")
			}
		}
	}

	// RoleBinding binds the Role to the squad SA only (AC2).
	var binding rbacv1.RoleBinding
	if err := c.Get(context.Background(), types.NamespacedName{Name: AgentServiceAccount, Namespace: ns}, &binding); err != nil {
		t.Fatalf("get RoleBinding: %v", err)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != rbacv1.ServiceAccountKind || binding.Subjects[0].Name != AgentServiceAccount {
		t.Errorf("RoleBinding subjects = %v, want exactly the squad SA", binding.Subjects)
	}
	if binding.RoleRef.Kind != "Role" {
		t.Errorf("RoleRef.Kind = %q, want Role (never ClusterRole)", binding.RoleRef.Kind)
	}

	// Quota + LimitRange both exist (AC3).
	var quota corev1.ResourceQuota
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ksquad-squad", Namespace: ns}, &quota); err != nil {
		t.Fatalf("get ResourceQuota (AC3): %v", err)
	}
	var limitRange corev1.LimitRange
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ksquad-squad", Namespace: ns}, &limitRange); err != nil {
		t.Fatalf("get LimitRange (AC3): %v", err)
	}
	sawPVCBounds := false
	for _, item := range limitRange.Spec.Limits {
		if item.Type == corev1.LimitTypePersistentVolumeClaim {
			sawPVCBounds = true
		}
	}
	if !sawPVCBounds {
		t.Errorf("LimitRange lacks PVC min/max bounds (AC3)")
	}

	// NetworkPolicy baseline: default-deny + allow-DNS + allow-control-plane
	// — all three must exist (AC4).
	var defaultDeny, allowDNS, allowCP networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ksquad-default-deny", Namespace: ns}, &defaultDeny); err != nil {
		t.Fatalf("get default-deny NetworkPolicy (AC4): %v", err)
	}
	if len(defaultDeny.Spec.PolicyTypes) != 2 {
		t.Errorf("default-deny policyTypes = %v, want [Ingress Egress]", defaultDeny.Spec.PolicyTypes)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ksquad-allow-dns", Namespace: ns}, &allowDNS); err != nil {
		t.Fatalf("get allow-dns NetworkPolicy (AC4): %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ksquad-allow-control-plane", Namespace: ns}, &allowCP); err != nil {
		t.Fatalf("get allow-control-plane NetworkPolicy (AC4): %v", err)
	}

	// Nothing was provisioned into ksquad-system (AC7) — assert on the
	// listed items, not just the list call (F9: the assertion used to be
	// decorative — the list result was never inspected).
	var sysPods corev1.PodList
	if err := c.List(context.Background(), &sysPods, client.InNamespace(SystemNamespace)); err != nil {
		t.Fatalf("list ksquad-system: %v", err)
	}
	if len(sysPods.Items) != 0 {
		t.Errorf("pods provisioned into %s (AC7 violation): %v", SystemNamespace, sysPods.Items)
	}
	var sysSAs corev1.ServiceAccountList
	if err := c.List(context.Background(), &sysSAs, client.InNamespace(SystemNamespace)); err != nil {
		t.Fatalf("list ksquad-system SAs: %v", err)
	}
	if len(sysSAs.Items) != 0 {
		t.Errorf("service accounts provisioned into %s (AC7 violation): %v", SystemNamespace, sysSAs.Items)
	}

	// Readiness is legible (AC5).
	var teamAgain api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &teamAgain)
	if !hasConditionTrue(teamAgain.Status.Conditions, condNamespaceReady) {
		t.Errorf("NamespaceReady condition not true after reconcile: %v", teamAgain.Status.Conditions)
	}
	if !containsString(FinalizerTenancy, teamAgain.Finalizers) {
		t.Errorf("tenancy finalizer not added")
	}
}

func hasConditionTrue(conds []metav1.Condition, condType string) bool {
	for _, c := range conds {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// TestProvisionIdempotent (AC5): re-reconciling converges with a single
// namespace and single copies of every object.
func TestProvisionIdempotent(t *testing.T) {
	r, c := newReconciler(t, newTeam("alpha", "uid-alpha"))

	for i := 0; i < 3; i++ {
		if err := reconcileTeam(t, r, "alpha"); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	var namespaces corev1.NamespaceList
	if err := c.List(context.Background(), &namespaces); err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	var count int
	for _, ns := range namespaces.Items {
		if ns.Labels[LabelTenancy] == TenancySquad {
			count++
		}
	}
	if count != 1 {
		t.Errorf("squad namespaces after 3 reconciles = %d, want 1 (AC5)", count)
	}

	var team api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &team)
	var sas corev1.ServiceAccountList
	if err := c.List(context.Background(), &sas, client.InNamespace(team.Status.Namespace)); err != nil {
		t.Fatalf("list SAs: %v", err)
	}
	if len(sas.Items) != 1 {
		t.Errorf("SAs in squad namespace = %d, want 1 (AC5)", len(sas.Items))
	}
}

// TestTwoTeamsDistinctNamespaces (AC1): distinct Teams reconcile into
// distinct namespaces; no control-plane object is mutated when a squad is
// added (NFR-SCALE1).
func TestTwoTeamsDistinctNamespaces(t *testing.T) {
	r, c := newReconciler(t, newTeam("alpha", "uid-alpha"), newTeam("beta", "uid-beta"))

	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile alpha: %v", err)
	}
	if err := reconcileTeam(t, r, "beta"); err != nil {
		t.Fatalf("reconcile beta: %v", err)
	}

	var alpha, beta api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &alpha)
	_ = c.Get(context.Background(), types.NamespacedName{Name: "beta", Namespace: "default"}, &beta)
	if alpha.Status.Namespace == "" || beta.Status.Namespace == "" || alpha.Status.Namespace == beta.Status.Namespace {
		t.Fatalf("alpha=%q beta=%q: namespaces must both exist and differ (AC1)", alpha.Status.Namespace, beta.Status.Namespace)
	}
}

// TestTeardownFinalizer (AC6): Team deletion removes the namespace; the
// finalizer clears only after the namespace is gone. While the namespace is
// Terminating (injected finalizer on a child wedges it), the Team finalizer
// is held and NamespaceTerminating is set.
func TestTeardownFinalizer(t *testing.T) {
	r, c := newReconciler(t, newTeam("alpha", "uid-alpha"))

	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var team api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &team)
	ns := team.Status.Namespace

	// Wedge the namespace in Terminating: a finalizer on the namespace
	// itself keeps it from disappearing (the fake client honors finalizers;
	// this is the AC6 stuck-Terminating path — a namespace whose content
	// won't drain stays Terminating).
	var namespace corev1.Namespace
	if err := c.Get(context.Background(), types.NamespacedName{Name: ns}, &namespace); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	namespace.Finalizers = []string{"example.com/wedge"}
	if err := c.Update(context.Background(), &namespace); err != nil {
		t.Fatalf("wedge namespace: %v", err)
	}

	// Delete the Team.
	if err := c.Delete(context.Background(), &team); err != nil {
		t.Fatalf("delete team: %v", err)
	}

	// Reconcile the deleting Team: it must delete the namespace...
	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile deleting team: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: ns}, &namespace); err != nil {
		t.Fatalf("namespace vanished immediately: %v", err)
	}

	// ...but HOLD the finalizer while the namespace is still present, and
	// surface NamespaceTerminating (AC6: never clear while Terminating).
	var held api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &held)
	if !containsString(FinalizerTenancy, held.Finalizers) {
		t.Fatalf("finalizer cleared while namespace still exists (AC6 violation)")
	}
	if !hasConditionTrue(held.Status.Conditions, condNamespaceTerminating) {
		t.Errorf("NamespaceTerminating condition not set: %v", held.Status.Conditions)
	}

	// Now unwedge (clear the namespace finalizer so the delete can
	// complete); the next reconcile clears the Team finalizer.
	namespace.Finalizers = nil
	if err := c.Update(context.Background(), &namespace); err != nil {
		t.Fatalf("clear wedge finalizer: %v", err)
	}
	if err := c.Delete(context.Background(), &namespace); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete namespace: %v", err)
	}

	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("final reconcile: %v", err)
	}
	var cleared api.Team
	err := c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &cleared)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("team should be gone after finalizer cleared, got err=%v finalizers=%v", err, cleared.Finalizers)
	}
}

// TestForeignNamespaceFailClosed (AC7 discipline): a namespace occupying the
// derived name without the tenancy labels is never adopted — the reconcile
// errors and records a conflict condition instead of writing into it.
func TestForeignNamespaceFailClosed(t *testing.T) {
	team := newTeam("alpha", "uid-alpha")
	nsName := NamespaceNameFor(team)
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}} // no tenancy labels
	r, c := newReconciler(t, team, foreign)

	if err := reconcileTeam(t, r, "alpha"); err == nil {
		t.Fatalf("reconcile over a foreign namespace must fail closed")
	}

	var after api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &after)
	if !hasConditionTrue(after.Status.Conditions, condNamespaceConflict) {
		t.Errorf("NamespaceConflict condition not recorded: %v", after.Status.Conditions)
	}

	// Nothing was provisioned into the foreign namespace.
	var sas corev1.ServiceAccountList
	if err := c.List(context.Background(), &sas, client.InNamespace(nsName)); err != nil {
		t.Fatalf("list SAs: %v", err)
	}
	if len(sas.Items) != 0 {
		t.Errorf("foreign namespace was adopted (SAs provisioned): %v", sas.Items)
	}
}

// TestScaffoldSelfHealing (Cursor review): with a namespace carrying a real
// UID, owner references are actually stamped on scaffold objects; a
// steady-state reconcile writes NOTHING (resourceVersion stable — the AC5
// claim, proven on writes rather than object counts); a deleted
// default-deny NetworkPolicy is recreated; a stripped team label is
// restored; and the namespace carries the restricted PSA labels.
func TestScaffoldSelfHealing(t *testing.T) {
	team := newTeam("alpha", "uid-alpha")
	nsName := NamespaceNameFor(team)
	// Pre-create the squad namespace WITH a UID (the fake client never
	// mints one), so the owner-reference path is actually exercised.
	managed := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: nsName,
		UID:  types.UID("ns-uid-real"),
		Labels: map[string]string{
			LabelTeam:    "alpha",
			LabelTenancy: TenancySquad,
		},
	}}
	r, c := newReconciler(t, team, managed)

	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Namespace carries the restricted Pod Security Standard labels.
	var namespace corev1.Namespace
	if err := c.Get(context.Background(), types.NamespacedName{Name: nsName}, &namespace); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if namespace.Labels[psaEnforceLabel] != "restricted" {
		t.Errorf("namespace %s label = %q, want restricted (untrusted code runs here)", psaEnforceLabel, namespace.Labels[psaEnforceLabel])
	}

	// Scaffold objects carry the namespace owner reference (cascade-delete
	// basis of the teardown design — dead code until this test gave the
	// namespace a UID).
	var sa corev1.ServiceAccount
	if err := c.Get(context.Background(), types.NamespacedName{Name: AgentServiceAccount, Namespace: nsName}, &sa); err != nil {
		t.Fatalf("get SA: %v", err)
	}
	if !hasNamespaceOwner(&sa, types.UID("ns-uid-real")) {
		t.Errorf("SA lacks owner reference to the squad namespace UID: %+v", sa.OwnerReferences)
	}

	// The agent Role grants NOTHING (Cursor review: namespace-wide pod and
	// pod-log reads on the shared squad SA were latent cross-principal
	// disclosure with zero functional benefit — the SA mounts no token).
	var role rbacv1.Role
	if err := c.Get(context.Background(), types.NamespacedName{Name: AgentServiceAccount, Namespace: nsName}, &role); err != nil {
		t.Fatalf("get Role: %v", err)
	}
	if len(role.Rules) != 0 {
		t.Errorf("agent Role rules = %+v, want the empty least-privilege floor", role.Rules)
	}

	// Steady state writes nothing: resourceVersion of every scaffold
	// object is byte-stable across further reconciles.
	snapshot := func() map[string]string {
		out := map[string]string{}
		for _, sa := range mustList[corev1.ServiceAccountList](t, c, nsName).Items {
			out["sa/"+sa.Name] = sa.ResourceVersion
		}
		for _, ro := range mustList[rbacv1.RoleList](t, c, nsName).Items {
			out["role/"+ro.Name] = ro.ResourceVersion
		}
		for _, rb := range mustList[rbacv1.RoleBindingList](t, c, nsName).Items {
			out["rbac/"+rb.Name] = rb.ResourceVersion
		}
		for _, q := range mustList[corev1.ResourceQuotaList](t, c, nsName).Items {
			out["quota/"+q.Name] = q.ResourceVersion
		}
		for _, lr := range mustList[corev1.LimitRangeList](t, c, nsName).Items {
			out["lr/"+lr.Name] = lr.ResourceVersion
		}
		for _, np := range mustList[networkingv1.NetworkPolicyList](t, c, nsName).Items {
			out["np/"+np.Name] = np.ResourceVersion
		}
		return out
	}
	before := snapshot()
	for i := 0; i < 2; i++ {
		if err := reconcileTeam(t, r, "alpha"); err != nil {
			t.Fatalf("steady-state reconcile %d: %v", i, err)
		}
	}
	after := snapshot()
	if len(before) == 0 {
		t.Fatalf("no scaffold objects found in %s", nsName)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("steady-state reconcile wrote %s (resourceVersion %s -> %s) — AC5 claims zero writes", k, v, after[k])
		}
	}

	// Self-heal: delete the default-deny NetworkPolicy; the next reconcile
	// must bring it back (the namespace must not run wide open until the
	// next Team write).
	deny := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "ksquad-default-deny", Namespace: nsName}}
	if err := c.Delete(context.Background(), deny); err != nil {
		t.Fatalf("delete default-deny: %v", err)
	}
	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile after scaffold delete: %v", err)
	}
	var restored networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ksquad-default-deny", Namespace: nsName}, &restored); err != nil {
		t.Fatalf("default-deny NetworkPolicy not self-healed: %v", err)
	}
	if len(restored.Spec.PolicyTypes) != 2 {
		t.Errorf("restored default-deny policyTypes = %v, want both Ingress and Egress", restored.Spec.PolicyTypes)
	}

	// Label drift restores: a stripped team label must not stay stripped
	// (it is the tenancy filter the Watches mapper keys on).
	role.Labels = map[string]string{"someone-else": "yes"}
	if err := c.Update(context.Background(), &role); err != nil {
		t.Fatalf("strip role labels: %v", err)
	}
	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile after label strip: %v", err)
	}
	var healed rbacv1.Role
	if err := c.Get(context.Background(), types.NamespacedName{Name: AgentServiceAccount, Namespace: nsName}, &healed); err != nil {
		t.Fatalf("get role: %v", err)
	}
	if healed.Labels[LabelTeam] != "alpha" || healed.Labels[LabelTeamNamespace] != "default" {
		t.Errorf("stripped team labels not restored: %+v", healed.Labels)
	}
	if healed.Labels["someone-else"] != "yes" {
		t.Errorf("foreign label wiped by label restore: %+v", healed.Labels)
	}
}

func mustList[L any, PL interface {
	client.ObjectList
	*L
}](t *testing.T, c client.Client, nsName string) *L {
	t.Helper()
	var l L
	if err := c.List(context.Background(), PL(&l), client.InNamespace(nsName)); err != nil {
		t.Fatalf("list %T: %v", l, err)
	}
	return &l
}

// TestTeardownTenancyLabelAsymmetry (PR #89 follow-up F1): provision refuses
// to adopt a namespace whose team label matches but whose TENANCY label
// does not — teardown must refuse symmetrically. The pre-F1 check looked
// only at ksquad.io/team, so exactly that namespace (adoption refused,
// then Team deleted) would have been DELETED on teardown.
func TestTeardownTenancyLabelAsymmetry(t *testing.T) {
	team := newTeam("alpha", "uid-alpha")
	nsName := NamespaceNameFor(team)
	// Team label right, tenancy label wrong/missing: adoption is refused
	// (AC7 discipline) — and by symmetry, so must deletion be.
	foreignish := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   nsName,
		Labels: map[string]string{LabelTeam: "alpha"},
	}}
	r, c := newReconciler(t, team, foreignish)

	if err := reconcileTeam(t, r, "alpha"); err == nil {
		t.Fatalf("provision over a wrong-tenancy namespace must fail closed")
	}

	// Delete the Team and run teardown.
	var teamObj api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &teamObj)
	if err := c.Delete(context.Background(), &teamObj); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("teardown reconcile: %v", err)
	}

	// The namespace must still exist (only its tenancy label was wrong —
	// it was never ours to delete), and the Team must be released.
	var ns corev1.Namespace
	if err := c.Get(context.Background(), types.NamespacedName{Name: nsName}, &ns); err != nil {
		t.Fatalf("wrong-tenancy namespace deleted on teardown (F1 asymmetry): %v", err)
	}
	var cleared api.Team
	err := c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &cleared)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("team not released after teardown: err=%v finalizers=%v", err, cleared.Finalizers)
	}
}

// TestHealedConflictClearsCondition (PR #89 follow-up F3): a NamespaceConflict
// persisted next to NamespaceReady=True must be REMOVED once the conflict
// heals, even when the ready condition itself does not change — condition
// removal has to count as a status change, or the fail-closed condition
// lingers forever (the pre-F3 write gate skipped the Update).
func TestHealedConflictClearsCondition(t *testing.T) {
	team := newTeam("alpha", "uid-alpha")
	nsName := NamespaceNameFor(team)
	r, c := newReconciler(t, team)

	// (a) Provision cleanly: NamespaceReady=True, ObservedGeneration stamped.
	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	// (b) Replace the namespace with a foreign squatter: the next reconcile
	// fails closed and persists NamespaceConflict (Ready stays True).
	var ns corev1.Namespace
	if err := c.Get(context.Background(), types.NamespacedName{Name: nsName}, &ns); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if err := c.Delete(context.Background(), &ns); err != nil {
		t.Fatalf("delete managed namespace: %v", err)
	}
	squatter := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	if err := c.Create(context.Background(), squatter); err != nil {
		t.Fatalf("create squatter namespace: %v", err)
	}
	if err := reconcileTeam(t, r, "alpha"); err == nil {
		t.Fatalf("reconcile over squatter must fail closed")
	}
	var conflicted api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &conflicted)
	if !hasConditionTrue(conflicted.Status.Conditions, condNamespaceConflict) {
		t.Fatalf("NamespaceConflict not recorded: %v", conflicted.Status.Conditions)
	}

	// (c) Heal: remove the squatter; the next reconcile succeeds with an
	// UNCHANGED ready condition and equal generations — exactly the shape
	// where the pre-F3 gate issued no status write.
	if err := c.Delete(context.Background(), squatter); err != nil {
		t.Fatalf("delete squatter: %v", err)
	}
	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("healed reconcile: %v", err)
	}

	var healed api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &healed)
	for _, cond := range healed.Status.Conditions {
		if cond.Type == condNamespaceConflict {
			t.Errorf("NamespaceConflict lingered after heal: %+v", healed.Status.Conditions)
		}
	}
	if !hasConditionTrue(healed.Status.Conditions, condNamespaceReady) {
		t.Errorf("NamespaceReady not true after heal: %v", healed.Status.Conditions)
	}
}

// TestProvisionIntoTerminatingNamespaceWaits (PR #89 follow-up F8):
// provisioning into a managed namespace that is Terminating surfaces a
// dedicated condition (reason WaitForGone) and requeues — never the opaque
// "unable to create new content in namespace … terminating" API-error
// loop. Once the namespace is fully gone, the next reconcile provisions
// cleanly and the condition clears.
func TestProvisionIntoTerminatingNamespaceWaits(t *testing.T) {
	team := newTeam("alpha", "uid-alpha")
	nsName := NamespaceNameFor(team)
	// A managed namespace wedged in Terminating (its own finalizer keeps it
	// from disappearing).
	terminating := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:       nsName,
		Labels:     map[string]string{LabelTeam: "alpha", LabelTenancy: TenancySquad},
		Finalizers: []string{"example.com/wedge"},
	}}
	r, c := newReconciler(t, team, terminating)
	if err := c.Delete(context.Background(), terminating); err != nil {
		t.Fatalf("start namespace deletion: %v", err)
	}

	// First reconcile: no error, a wait-for-gone requeue, the condition set
	// and NamespaceReady absent.
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "alpha", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile over terminating namespace errored (want wait-for-gone requeue): %v", err)
	}
	if res.RequeueAfter != terminatingRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, terminatingRequeue)
	}
	var waiting api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &waiting)
	sawTerminating, sawReady := false, false
	for _, cond := range waiting.Status.Conditions {
		if cond.Type == condNamespaceTerminating && cond.Status == metav1.ConditionTrue && cond.Reason == "WaitForGone" {
			sawTerminating = true
		}
		if cond.Type == condNamespaceReady {
			sawReady = true
		}
	}
	if !sawTerminating {
		t.Errorf("NamespaceTerminating/WaitForGone condition not recorded: %v", waiting.Status.Conditions)
	}
	if sawReady {
		t.Errorf("NamespaceReady=True persisted next to a terminating namespace: %v", waiting.Status.Conditions)
	}

	// Nothing was provisioned into the draining namespace.
	var sas corev1.ServiceAccountList
	if err := c.List(context.Background(), &sas, client.InNamespace(nsName)); err != nil {
		t.Fatalf("list SAs: %v", err)
	}
	if len(sas.Items) != 0 {
		t.Errorf("scaffold provisioned into a Terminating namespace: %v", sas.Items)
	}

	// Unwedge and fully remove the namespace; the next reconcile
	// provisions fresh and clears the terminating condition.
	var wedged corev1.Namespace
	if err := c.Get(context.Background(), types.NamespacedName{Name: nsName}, &wedged); err != nil {
		t.Fatalf("get wedged namespace: %v", err)
	}
	wedged.Finalizers = nil
	if err := c.Update(context.Background(), &wedged); err != nil {
		t.Fatalf("clear wedge finalizer: %v", err)
	}
	if err := c.Delete(context.Background(), &wedged); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("finish namespace deletion: %v", err)
	}
	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("post-gone reconcile: %v", err)
	}
	var fresh api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &fresh)
	for _, cond := range fresh.Status.Conditions {
		if cond.Type == condNamespaceTerminating {
			t.Errorf("NamespaceTerminating lingered after clean provision: %+v", fresh.Status.Conditions)
		}
	}
	if !hasConditionTrue(fresh.Status.Conditions, condNamespaceReady) {
		t.Errorf("NamespaceReady not true after clean provision: %v", fresh.Status.Conditions)
	}
}

// TestControlPlaneEgressPortScoped (PR #89 follow-up F4): the
// allow-control-plane companion must be port-scoped like the DNS companion
// — TCP 443/8080 only — so a squad cannot reach Postgres (or anything
// else) that happens to live in the control-plane namespace.
func TestControlPlaneEgressPortScoped(t *testing.T) {
	r, c := newReconciler(t, newTeam("alpha", "uid-alpha"))
	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var team api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &team)

	var allowCP networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ksquad-allow-control-plane", Namespace: team.Status.Namespace}, &allowCP); err != nil {
		t.Fatalf("get allow-control-plane NetworkPolicy: %v", err)
	}
	if len(allowCP.Spec.Egress) != 1 {
		t.Fatalf("egress rules = %d, want 1", len(allowCP.Spec.Egress))
	}
	rule := allowCP.Spec.Egress[0]
	if len(rule.Ports) == 0 {
		t.Fatalf("control-plane egress is not port-scoped (any port/protocol to the whole namespace, F4)")
	}
	saw443, saw8080 := false, false
	for _, p := range rule.Ports {
		if p.Protocol == nil || *p.Protocol != tcpProtocol || p.Port == nil {
			t.Errorf("non-TCP or portless egress entry: %+v", p)
			continue
		}
		switch p.Port.IntValue() {
		case 443:
			saw443 = true
		case 8080:
			saw8080 = true
		default:
			t.Errorf("unexpected control-plane egress port %v (F4 allowlist is 443/8080)", p.Port)
		}
	}
	if !saw443 || !saw8080 {
		t.Errorf("control-plane egress ports missing 443/8080: %+v", rule.Ports)
	}
}

// TestConditionTransitionTimePinnedByClock (PR #89 follow-up F9): the
// Reconciler.Now clock is actually wired into condition transitions — a
// frozen clock produces a frozen LastTransitionTime instead of the wall
// clock (meta.SetStatusCondition honors a pre-set LastTransitionTime).
func TestConditionTransitionTimePinnedByClock(t *testing.T) {
	frozen := metav1.Time{Time: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(newTeam("alpha", "uid-alpha")).WithStatusSubresource(&api.Team{}).Build()
	r := &Reconciler{Client: c, Now: func() metav1.Time { return frozen }}

	if err := reconcileTeam(t, r, "alpha"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var team api.Team
	_ = c.Get(context.Background(), types.NamespacedName{Name: "alpha", Namespace: "default"}, &team)
	for _, cond := range team.Status.Conditions {
		if cond.Type == condNamespaceReady && !cond.LastTransitionTime.Equal(&frozen) {
			t.Errorf("NamespaceReady LastTransitionTime = %v, want the frozen clock %v (Now field not wired)", cond.LastTransitionTime, frozen)
		}
	}
}
