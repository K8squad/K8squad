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
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/toolchain"
)

// Epic B (ISI-3286) toolchain RBAC rendering (plan §2.2b): the operator —
// never the user — renders the unioned RBAC of a Run's resolved
// toolchains as one per-Run Role bound to the managed squad
// ServiceAccount, garbage-collected with the Run. Deploying a tool for
// squads stays "apply one Toolchain object": no Role, no RoleBinding, no
// SA plumbing to hand-write.

const (
	// RunRBACNamePrefix is the per-Run rendered object name prefix:
	// ksquad-run-<run-name>.
	RunRBACNamePrefix = "ksquad-run-"

	// AgentServiceAccount mirrors pkg/controller/team.AgentServiceAccount
	// (the managed squad SA every sandbox pod runs as, story 4.2). Mirrored
	// locally to keep the run controller free of the team controller's
	// dependency graph — the team controller does the same in reverse.
	AgentServiceAccount = "ksquad-agent"

	// LabelRunOwnerNamespace carries the owning Run's namespace on
	// cluster-scoped rendered objects (a ClusterRole cannot carry a
	// namespaced ownerReference, so Release/sweep discovery uses labels).
	LabelRunOwnerNamespace = "ksquad.io/run-namespace"
)

// labelRun / labelManagedBy match the Team reconciler's managed label
// dialect so every operator-owned object reads uniformly in the console.
const (
	labelRun       = "ksquad.io/run"
	labelManagedBy = "app.kubernetes.io/managed-by"
)

// RBACRenderer renders and repairs the per-Run toolchain RBAC surface.
// It is deliberately separate from the status projector above: assembly
// (Epic C) will reuse the same component at pod-build time, so the grant
// admission proved and the grant dispatch assumes are rendered by one
// code path.
type RBACRenderer struct {
	client.Client
	// Platform carries the cluster-catalog namespace and the
	// cluster-scope opt-in (deployment env, Helm values).
	Platform toolchain.PlatformConfig
}

// NewRBACRenderer builds a renderer over a manager-managed or fake client.
func NewRBACRenderer(c client.Client, platform toolchain.PlatformConfig) *RBACRenderer {
	return &RBACRenderer{Client: c, Platform: platform}
}

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

// Ensure resolves the Run's toolchain demand and converges the rendered
// RBAC objects to it:
//
//   - namespace-scope rules (the default posture) union into a per-Run
//     Role + RoleBinding bound to the managed ksquad-agent SA, owned by
//     the Run (Kubernetes GC deletes them with it);
//   - cluster-scope rules (catalog entries behind the platform opt-in)
//     union into a per-Run ClusterRole + ClusterRoleBinding — no
//     ownerReference is possible (namespaced owner, cluster-scoped
//     dependent), so terminal-phase Release plus the owner labels carry
//     cleanup;
//   - the exact union is returned for recording on Run.status
//     (GrantedToolchainRBAC), the audit of which Run got which
//     permissions through which toolchain.
//
// A Run with no toolchain demand returns a nil grant: nothing is
// rendered, nothing recorded. Resolution failures error (fail-closed
// requeue) — a Run never proceeds with partial RBAC.
func (r *RBACRenderer) Ensure(ctx context.Context, run *api.Run) (*api.ToolchainRBACGrant, error) {
	platform := r.Platform.WithDefaults()
	resolver := &toolchain.Resolver{Reader: r.Client, Platform: platform}

	reqs, err := resolver.RefsForRun(ctx, run)
	if err != nil {
		return nil, err
	}
	if len(reqs.Refs) == 0 {
		return nil, nil
	}
	resolved, err := resolver.ResolveRefs(ctx, run.Namespace, reqs.Refs, toolchain.DetailsFor(run))
	if err != nil {
		return nil, err
	}

	var namespaceSets, clusterSets [][]rbacv1.PolicyRule
	for _, res := range resolved {
		if res.RBAC == nil || len(res.RBAC.Rules) == 0 {
			continue
		}
		if res.RBAC.Scope == api.ToolchainRBACScopeCluster {
			if !platform.AllowClusterScope {
				return nil, fmt.Errorf("toolchain %s@%s carries cluster-scope RBAC but the platform opt-in (%s) is off; refusing to render, fail-closed",
					res.Name, res.Version, toolchain.EnvAllowClusterScope)
			}
			clusterSets = append(clusterSets, res.RBAC.Rules)
			continue
		}
		namespaceSets = append(namespaceSets, res.RBAC.Rules)
	}

	grant := &api.ToolchainRBACGrant{
		Toolchains: make([]api.ResolvedToolchainRef, 0, len(resolved)),
	}
	for _, res := range resolved {
		grant.Toolchains = append(grant.Toolchains, api.ResolvedToolchainRef{
			Name:            res.Name,
			Version:         res.Version,
			Image:           res.Image,
			SourceNamespace: res.SourceNamespace,
		})
	}

	if rules := toolchain.UnionRules(namespaceSets...); len(rules) > 0 {
		roleRef, err := r.ensureNamespaced(ctx, run, rules)
		if err != nil {
			return nil, err
		}
		grant.RoleRef = roleRef
		grant.Rules = append(grant.Rules, rules...)
	}
	if rules := toolchain.UnionRules(clusterSets...); len(rules) > 0 {
		roleRef, err := r.ensureClusterScoped(ctx, run, rules)
		if err != nil {
			return nil, err
		}
		grant.ClusterRoleRef = roleRef
		grant.Rules = append(grant.Rules, rules...)
	}
	return grant, nil
}

// Release garbage-collects the rendered RBAC surface when a Run goes
// terminal (acceptance 3b: the per-Run Role disappears on Run
// completion). Idempotent — a requeued terminal Run reconciles to a
// no-op. Run-object deletion is covered separately by the ownerReference
// GC on the namespaced objects.
func (r *RBACRenderer) Release(ctx context.Context, run *api.Run) error {
	name := RunRBACNamePrefix + run.Name
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: name}}
	if err := r.Delete(ctx, role); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete run role %s/%s: %w", run.Namespace, name, err)
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: name}}
	if err := r.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete run rolebinding %s/%s: %w", run.Namespace, name, err)
	}
	clusterName := runClusterObjectName(run)
	crole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: clusterName}}
	if err := r.Delete(ctx, crole); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete run clusterrole %s: %w", clusterName, err)
	}
	cbinding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterName}}
	if err := r.Delete(ctx, cbinding); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete run clusterrolebinding %s: %w", clusterName, err)
	}
	return nil
}

// ensureNamespaced converges the per-Run Role + RoleBinding in the Run's
// namespace: unioned rules, managed labels, and a controller
// ownerReference to the Run so Kubernetes GC deletes them with it.
func (r *RBACRenderer) ensureNamespaced(ctx context.Context, run *api.Run, rules []rbacv1.PolicyRule) (*api.ObjectRef, error) {
	name := RunRBACNamePrefix + run.Name

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = runRBACLabels(run)
		role.Rules = rules
		setRunOwner(run, role)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ensure run role %s/%s: %w", run.Namespace, name, err)
	}

	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = runRBACLabels(run)
		binding.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Namespace: run.Namespace,
			Name:      AgentServiceAccount,
		}}
		binding.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name,
		}
		setRunOwner(run, binding)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ensure run rolebinding %s/%s: %w", run.Namespace, name, err)
	}

	return &api.ObjectRef{Namespace: run.Namespace, Name: name}, nil
}

// ensureClusterScoped converges the per-Run ClusterRole + binding for
// catalog entries that legitimately need cluster reach (platform opt-in
// only — Ensure already fail-closed otherwise). A cluster-scoped object
// cannot own a namespaced ownerReference, so cleanup rides Release plus
// the owner labels; an orphaned ClusterRole is discoverable and
// re-deletable via ksquad.io/run + ksquad.io/run-namespace.
func (r *RBACRenderer) ensureClusterScoped(ctx context.Context, run *api.Run, rules []rbacv1.PolicyRule) (*api.ObjectRef, error) {
	name := runClusterObjectName(run)

	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = runRBACLabels(run)
		role.Rules = rules
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ensure run clusterrole %s: %w", name, err)
	}

	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = runRBACLabels(run)
		binding.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Namespace: run.Namespace,
			Name:      AgentServiceAccount,
		}}
		binding.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     name,
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ensure run clusterrolebinding %s: %w", name, err)
	}

	return &api.ObjectRef{Namespace: "", Name: name}, nil
}

// runClusterObjectName derives the cluster-unique name for cluster-scoped
// rendered objects: ksquad-run-<namespace>-<name>, truncated to the
// DNS-subdomain limit (ClusterRole names are cluster-unique; the
// namespace prefix keeps two same-named Runs in different namespaces from
// colliding).
func runClusterObjectName(run *api.Run) string {
	name := RunRBACNamePrefix + run.Namespace + "-" + run.Name
	const maxDNS = 253
	if len(name) > maxDNS {
		name = name[:maxDNS]
	}
	return name
}

// runRBACLabels stamps the managed dialect plus the owning Run identity
// (the sweep/discovery surface for cluster-scoped objects).
func runRBACLabels(run *api.Run) map[string]string {
	return map[string]string{
		labelManagedBy:         "ksquad-operator",
		labelRun:               run.Name,
		LabelRunOwnerNamespace: run.Namespace,
	}
}

// setRunOwner stamps the controller ownerReference tying namespaced
// rendered objects to the Run (Kubernetes GC deletes them with it).
func setRunOwner(run *api.Run, obj metav1.Object) {
	ref := metav1.NewControllerRef(run, api.GroupVersion.WithKind("Run"))
	obj.SetOwnerReferences([]metav1.OwnerReference{*ref})
}

// isTerminalPhase reports whether the phase is absorbing (arch §8): no
// further assembly is owed and the rendered RBAC must be released.
func isTerminalPhase(p api.RunPhase) bool {
	switch p {
	case api.RunPhaseSucceeded, api.RunPhaseFailed, api.RunPhaseCancelled:
		return true
	}
	return false
}
