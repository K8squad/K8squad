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

package run

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/K8squad/K8squad/pkg/toolchain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rbacScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := newScheme(t)
	require.NoError(t, clientgoscheme.AddToScheme(s))
	return s
}

func newRBACClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(rbacScheme(t)).
		WithObjects(objs...).WithStatusSubresource(&api.Run{}).Build()
}

// seedToolchainWorld builds the resolution chain Run → Agent → Skill →
// catalog: kubectl (read-only RBAC) + git (no RBAC) in the cluster
// catalog, demanded by one skill.
func seedToolchainWorld(t *testing.T, runNS string) ([]client.Object, *api.Run) {
	t.Helper()
	skill := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-deploy", Namespace: runNS},
		Spec: api.SkillSpec{Requires: api.SkillRequires{Toolchains: []string{
			"kubectl@1.31", "git@2.45",
		}}},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "amelia", Namespace: runNS},
		Spec:       api.AgentSpec{SkillRefs: []api.ObjectRef{{Name: skill.Name}}},
	}
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-42", Namespace: runNS, UID: types.UID("uid-42")},
		Spec:       api.RunSpec{WorkItemRef: "wi-1", Agents: []api.ObjectRef{{Name: agent.Name}}},
	}
	kubectl := &api.Toolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl", Namespace: toolchain.DefaultClusterCatalogNamespace},
		Spec: api.ToolchainSpec{
			Versions: []api.ToolchainVersion{{Version: "1.31", Image: "ghcr.io/k8squad/toolchains/kubectl:1.31"}},
			RBAC: &api.ToolchainRBAC{Scope: api.ToolchainRBACScopeNamespace, Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"pods", "services"}, Verbs: []string{"get", "list", "watch"}},
				{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get", "list", "watch", "patch"}},
			}},
		},
	}
	git := &api.Toolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: toolchain.DefaultClusterCatalogNamespace},
		Spec: api.ToolchainSpec{
			Versions: []api.ToolchainVersion{{Version: "2.45", Image: "ghcr.io/k8squad/toolchains/git:2.45"}},
		},
	}
	return []client.Object{skill, agent, kubectl, git}, run
}

// TestRendererEnsureRendersUnionRoleAndBinding: the per-Run Role carries
// exactly the unioned catalog rules (git contributes none), is bound to
// the managed ksquad-agent SA, is owned by the Run, and the grant records
// the provenance + union (acceptance 3b: `kubectl auth can-i` shows
// exactly the declared RBAC).
func TestRendererEnsureRendersUnionRoleAndBinding(t *testing.T) {
	objs, run := seedToolchainWorld(t, "bmad-squad")
	c := newRBACClient(t, objs...)
	r := NewRBACRenderer(c, toolchain.DefaultPlatformConfig())

	grant, err := r.Ensure(context.Background(), run)
	require.NoError(t, err)
	require.NotNil(t, grant)
	require.NotNil(t, grant.RoleRef)
	assert.Equal(t, "ksquad-run-run-42", grant.RoleRef.Name)
	assert.Equal(t, "bmad-squad", grant.RoleRef.Namespace)
	assert.Nil(t, grant.ClusterRoleRef, "namespace posture renders no cluster objects")

	require.Len(t, grant.Toolchains, 2)
	assert.Equal(t, "git", grant.Toolchains[0].Name)
	assert.Equal(t, "kubectl", grant.Toolchains[1].Name)
	require.Len(t, grant.Rules, 2, "git contributes no rules; duplicates dedupe")

	role := &rbacv1.Role{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "bmad-squad", Name: "ksquad-run-run-42"}, role))
	assert.Equal(t, grant.Rules, role.Rules, "the Role carries exactly the recorded union")
	assert.Equal(t, map[string]string{
		"app.kubernetes.io/managed-by": "ksquad-operator",
		"ksquad.io/run":                "run-42",
		"ksquad.io/run-namespace":      "bmad-squad",
	}, role.Labels)
	require.Len(t, role.OwnerReferences, 1)
	assert.Equal(t, "Run", role.OwnerReferences[0].Kind)
	assert.Equal(t, run.UID, role.OwnerReferences[0].UID)
	assert.True(t, *role.OwnerReferences[0].Controller, "Run controls its rendered Role")

	binding := &rbacv1.RoleBinding{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "bmad-squad", Name: "ksquad-run-run-42"}, binding))
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, rbacv1.ServiceAccountKind, binding.Subjects[0].Kind)
	assert.Equal(t, AgentServiceAccount, binding.Subjects[0].Name)
	assert.Equal(t, "bmad-squad", binding.Subjects[0].Namespace)
	assert.Equal(t, "ksquad-run-run-42", binding.RoleRef.Name)
}

// TestRendererEnsureIdempotentAndRepairs: a second pass with an unchanged
// world is a no-op; narrowing the catalog entry converges the Role to the
// narrower union.
func TestRendererEnsureIdempotentAndRepairs(t *testing.T) {
	objs, run := seedToolchainWorld(t, "bmad-squad")
	c := newRBACClient(t, objs...)
	r := NewRBACRenderer(c, toolchain.DefaultPlatformConfig())

	_, err := r.Ensure(context.Background(), run)
	require.NoError(t, err)
	grant2, err := r.Ensure(context.Background(), run)
	require.NoError(t, err)
	require.Len(t, grant2.Rules, 2)

	narrowed := objs[2].(*api.Toolchain) // kubectl
	narrowed.Spec.RBAC.Rules = narrowed.Spec.RBAC.Rules[:1]
	require.NoError(t, c.Update(context.Background(), narrowed))

	grant3, err := r.Ensure(context.Background(), run)
	require.NoError(t, err)
	require.Len(t, grant3.Rules, 1, "a narrowed catalog converges the rendered Role")
}

// TestRendererEnsureNoDemandNoObjects: a Run whose skills require nothing
// renders nothing — the empty-baseline posture is preserved.
func TestRendererEnsureNoDemandNoObjects(t *testing.T) {
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-plain", Namespace: "bmad-squad", UID: types.UID("uid-p")},
		Spec:       api.RunSpec{WorkItemRef: "wi-2"},
	}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "bmad-squad"}}
	c := newRBACClient(t, agent)
	r := NewRBACRenderer(c, toolchain.DefaultPlatformConfig())

	grant, err := r.Ensure(context.Background(), run)
	require.NoError(t, err)
	assert.Nil(t, grant, "no toolchain demand → no grant")

	roles := &rbacv1.RoleList{}
	require.NoError(t, c.List(context.Background(), roles))
	assert.Empty(t, roles.Items, "no Role may be created for a toolchain-less Run")
}

// TestRendererEnsureFailClosedOnUnknownRef: an unresolvable ref errors
// (requeue) instead of silently rendering a partial grant.
func TestRendererEnsureFailClosedOnUnknownRef(t *testing.T) {
	skill := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "wants-gh", Namespace: "bmad-squad"},
		Spec:       api.SkillSpec{Requires: api.SkillRequires{Toolchains: []string{"gh@2.62"}}},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "amelia", Namespace: "bmad-squad"},
		Spec:       api.AgentSpec{SkillRefs: []api.ObjectRef{{Name: "wants-gh"}}},
	}
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-x", Namespace: "bmad-squad"},
		Spec:       api.RunSpec{Agents: []api.ObjectRef{{Name: "amelia"}}},
	}
	c := newRBACClient(t, skill, agent)
	r := NewRBACRenderer(c, toolchain.DefaultPlatformConfig())

	_, err := r.Ensure(context.Background(), run)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Toolchain")
}

// TestRendererClusterScopeRespectsOptIn: cluster-scope rules render only
// behind the platform opt-in; without it, Ensure fails closed.
func TestRendererClusterScopeRespectsOptIn(t *testing.T) {
	skill := &api.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-peek", Namespace: "bmad-squad"},
		Spec:       api.SkillSpec{Requires: api.SkillRequires{Toolchains: []string{"kubectl@1.31"}}},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "amelia", Namespace: "bmad-squad"},
		Spec:       api.AgentSpec{SkillRefs: []api.ObjectRef{{Name: "cluster-peek"}}},
	}
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-c", Namespace: "bmad-squad", UID: types.UID("uid-c")},
		Spec:       api.RunSpec{Agents: []api.ObjectRef{{Name: "amelia"}}},
	}
	kubectl := &api.Toolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl", Namespace: toolchain.DefaultClusterCatalogNamespace},
		Spec: api.ToolchainSpec{
			Versions: []api.ToolchainVersion{{Version: "1.31", Image: "img:1.31"}},
			RBAC: &api.ToolchainRBAC{Scope: api.ToolchainRBACScopeCluster, Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list"}},
			}},
		},
	}
	c := newRBACClient(t, skill, agent, kubectl)

	_, err := NewRBACRenderer(c, toolchain.DefaultPlatformConfig()).Ensure(context.Background(), run)
	require.Error(t, err, "cluster scope without the opt-in must fail closed")
	assert.Contains(t, err.Error(), "opt-in")

	platform := toolchain.DefaultPlatformConfig()
	platform.AllowClusterScope = true
	grant, err := NewRBACRenderer(c, platform).Ensure(context.Background(), run)
	require.NoError(t, err)
	require.NotNil(t, grant.ClusterRoleRef)
	assert.Equal(t, "ksquad-run-bmad-squad-run-c", grant.ClusterRoleRef.Name)
	assert.Nil(t, grant.RoleRef, "no namespace-scope rules → no namespaced Role")

	crole := &rbacv1.ClusterRole{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "ksquad-run-bmad-squad-run-c"}, crole))
	require.Len(t, crole.Rules, 1)
	cbinding := &rbacv1.ClusterRoleBinding{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "ksquad-run-bmad-squad-run-c"}, cbinding))
	require.Len(t, cbinding.Subjects, 1)
	assert.Equal(t, AgentServiceAccount, cbinding.Subjects[0].Name)
}

// TestRendererReleaseGCsRenderedObjects: Release deletes the namespaced
// and cluster-scoped surface idempotently (acceptance 3b: the Role
// disappears on Run completion).
func TestRendererReleaseGCsRenderedObjects(t *testing.T) {
	objs, run := seedToolchainWorld(t, "bmad-squad")
	c := newRBACClient(t, objs...)
	r := NewRBACRenderer(c, toolchain.DefaultPlatformConfig())

	_, err := r.Ensure(context.Background(), run)
	require.NoError(t, err)

	require.NoError(t, r.Release(context.Background(), run))
	require.NoError(t, r.Release(context.Background(), run), "release is idempotent")

	err = c.Get(context.Background(), types.NamespacedName{Namespace: "bmad-squad", Name: "ksquad-run-run-42"}, &rbacv1.Role{})
	assert.Error(t, err, "Role must be gone")
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "bmad-squad", Name: "ksquad-run-run-42"}, &rbacv1.RoleBinding{})
	assert.Error(t, err, "RoleBinding must be gone")
}

// TestReconcilerConvergesToolchainRBAC: end to end through the reconciler
// — a live Run gets the grant recorded on status and the Role rendered; a
// terminal Run gets both released and cleared.
func TestReconcilerConvergesToolchainRBAC(t *testing.T) {
	objs, run := seedToolchainWorld(t, "bmad-squad")
	objs = append(objs, run)
	c := newRBACClient(t, objs...)

	r := &Reconciler{
		Client: c,
		Source: fakeSource{step: reconcile.StepRunning, found: true},
		RBAC:   NewRBACRenderer(c, toolchain.DefaultPlatformConfig()),
	}

	_, err := r.Reconcile(context.Background(), ctrlRequest(run))
	require.NoError(t, err)

	var after api.Run
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, &after))
	require.NotNil(t, after.Status.GrantedToolchainRBAC, "live Run records the grant")
	require.Len(t, after.Status.GrantedToolchainRBAC.Rules, 2)

	role := &rbacv1.Role{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "bmad-squad", Name: "ksquad-run-run-42"}, role))

	// Terminal step: the grant clears and the Role disappears.
	terminal := &Reconciler{
		Client: c,
		Source: fakeSource{step: reconcile.StepSucceeded, found: true},
		RBAC:   NewRBACRenderer(c, toolchain.DefaultPlatformConfig()),
	}
	_, err = terminal.Reconcile(context.Background(), ctrlRequest(run))
	require.NoError(t, err)

	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, &after))
	assert.Nil(t, after.Status.GrantedToolchainRBAC, "terminal Run clears the grant record")
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "bmad-squad", Name: "ksquad-run-run-42"}, &rbacv1.Role{})
	assert.Error(t, err, "terminal Run's Role is garbage-collected")
}

func ctrlRequest(run *api.Run) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}}
}
