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

// Package team implements the Story 4.1 squad-tenancy reconciler (arch §12.1,
// ADR-011): a Team IS a Kubernetes namespace. On every reconcile the
// controller provisions — create-or-update, idempotently — the namespace and
// its least-privilege scaffold (ServiceAccount, namespaced Role/RoleBinding,
// ResourceQuota, LimitRange, and the default-deny + allow-DNS +
// allow-control-plane NetworkPolicy baseline), and tears the namespace down
// finalizer-driven on Team deletion.
package team

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

const (
	// FinalizerTenancy guards the namespace teardown path (story 4.1 AC6). A
	// namespaced Team cannot own a cluster-scoped Namespace via
	// ownerReference (Kubernetes GC refuses it), so the finalizer is the only
	// mechanism that can reap the namespace on Team delete. It clears only
	// after the namespace is actually gone — never while it is Terminating.
	FinalizerTenancy = "ksquad.io/tenancy"

	// LabelTeam marks every object the Team reconciler owns, carrying the
	// Team name — the tenancy filter the memory/coord predicates and
	// NetworkPolicy selectors key on.
	LabelTeam = "ksquad.io/team"

	// LabelTeamNamespace carries the Team object's own namespace on every
	// scaffold object. The owner of the scaffold is the (cluster-scoped)
	// squad Namespace, so owner references cannot route events back to the
	// namespaced Team; this label lets the self-healing Watches map a
	// scaffold-object event to the exact Team reconcile request.
	LabelTeamNamespace = "ksquad.io/team-namespace"

	// LabelTenancy marks the tenancy kind of a namespace (squad vs system).
	LabelTenancy = "ksquad.io/tenancy"

	// TenancySquad is the LabelTenancy value of a squad (Team) namespace.
	TenancySquad = "squad"

	// Pod Security Standard labels stamped on every squad namespace: the
	// namespace runs untrusted agent code, so it enforces the restricted
	// profile — the cheapest single hardening available (admission-level,
	// before any pod is even created). The sandbox pods emitted by
	// pkg/sandbox comply (runAsNonRoot, seccomp, no privilege escalation,
	// all capabilities dropped).
	psaEnforceLabel        = "pod-security.kubernetes.io/enforce"
	psaEnforceVersionLabel = "pod-security.kubernetes.io/enforce-version"

	// AgentServiceAccount is the single namespaced ServiceAccount the squad's
	// agent workloads (sandbox pods, Story 4.2) run as. It carries no
	// cluster-scoped grants and no Secret access at all (see agentRole).
	AgentServiceAccount = "ksquad-agent"

	// SystemNamespace is the control-plane namespace (arch §4: operator,
	// apiserver, memory service, Postgres). The reconciler never provisions
	// into it and fail-closes if a Team would resolve onto it (AC7).
	SystemNamespace = "ksquad-system"

	// condNamespaceReady reports a fully provisioned squad namespace (AC5).
	condNamespaceReady = "NamespaceReady"
	// condNamespaceReserved reports a fail-closed reserved-namespace
	// resolution (AC7).
	condNamespaceReserved = "NamespaceReserved"
	// condNamespaceConflict reports a foreign namespace occupying the
	// derived name (fail-closed; never adopt an unmanaged namespace).
	condNamespaceConflict = "NamespaceConflict"
	// condNamespaceTerminating reports a namespace stuck/progressing through
	// Terminating during Team deletion (AC6) — the finalizer is not cleared
	// while this is active.
	condNamespaceTerminating = "NamespaceTerminating"
)

// Clock returns the timestamp stamped onto condition transitions. It is a
// field so tests pin it and a no-op requeue produces byte-identical status.
type Clock func() metav1.Time

// Reconciler provisions the §12.1 squad tenancy scaffold for each Team.
type Reconciler struct {
	client.Client
	// Now defaults to metav1.Now when nil.
	Now Clock
	// ControlPlaneNamespace is the namespace the allow-control-plane
	// NetworkPolicy opens egress to (the §12.2 companion). Defaults to
	// SystemNamespace; overridable for non-standard installs.
	ControlPlaneNamespace string
	// DNSNamespace is the namespace the allow-DNS companion opens egress to.
	// Defaults to kube-system (cluster DNS).
	DNSNamespace string
}

//+kubebuilder:rbac:groups=ksquad.io,resources=teams,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=ksquad.io,resources=teams/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ksquad.io,resources=teams/finalizers,verbs=update;patch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives a Team to its provisioned squad namespace (or through
// finalizer teardown on deletion). It is idempotent: a steady-state requeue
// converges with zero writes (AC5).
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var teamObj api.Team
	if err := r.Get(ctx, req.NamespacedName, &teamObj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Teardown path (AC6): the Team is being deleted — reap the namespace
	// before the finalizer clears.
	if !teamObj.DeletionTimestamp.IsZero() {
		return r.teardown(ctx, &teamObj)
	}

	// Provision path: add the finalizer before any object is created, so a
	// Team deleted mid-provision still tears its namespace down.
	if !containsString(FinalizerTenancy, teamObj.Finalizers) {
		patch := client.MergeFrom(teamObj.DeepCopy())
		teamObj.Finalizers = append(teamObj.Finalizers, FinalizerTenancy)
		if err := r.Patch(ctx, &teamObj, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("add tenancy finalizer: %w", err)
		}
	}

	nsName, err := r.resolveNamespace(ctx, &teamObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.provision(ctx, &teamObj, nsName); err != nil {
		return ctrl.Result{}, err
	}

	// Readiness is legible, never assumed (AC5).
	changed := setCondition(&teamObj, condNamespaceReady, metav1.ConditionTrue,
		"TenancyProvisioned", fmt.Sprintf("namespace %s provisioned with SA/RBAC/quota/LimitRange/NetworkPolicy baseline", nsName))
	clearConditions(&teamObj, condNamespaceReserved, condNamespaceConflict)
	if changed || teamObj.Status.ObservedGeneration != teamObj.Generation {
		teamObj.Status.ObservedGeneration = teamObj.Generation
		if err := r.Status().Update(ctx, &teamObj); err != nil {
			return ctrl.Result{}, fmt.Errorf("update team status: %w", err)
		}
	}
	log.V(1).Info("team tenancy reconciled", "namespace", nsName)
	return ctrl.Result{}, nil
}

// resolveNamespace returns the Team's squad namespace, deriving and recording
// it on first resolution and reading it back from status.namespace afterwards
// (AC1: rename-safe). It fail-closes on a reserved namespace (AC7).
func (r *Reconciler) resolveNamespace(ctx context.Context, teamObj *api.Team) (string, error) {
	if ns := teamObj.Status.Namespace; ns != "" {
		if IsReservedNamespace(ns) {
			setErrorCondition(ctx, r, teamObj, condNamespaceReserved,
				fmt.Sprintf("status.namespace %q is a reserved system namespace; refusing to provision (AC7)", ns))
			return "", fmt.Errorf("team %s: namespace %q is reserved", teamObj.Name, ns)
		}
		return ns, nil
	}

	ns := NamespaceNameFor(teamObj)
	if IsReservedNamespace(ns) {
		setErrorCondition(ctx, r, teamObj, condNamespaceReserved,
			fmt.Sprintf("derived namespace %q collides with a reserved system namespace; refusing to provision (AC7)", ns))
		return "", fmt.Errorf("team %s: derived namespace %q is reserved", teamObj.Name, ns)
	}

	patch := client.MergeFrom(teamObj.DeepCopy())
	teamObj.Status.Namespace = ns
	if err := r.Status().Patch(ctx, teamObj, patch); err != nil {
		return "", fmt.Errorf("record status.namespace: %w", err)
	}
	return ns, nil
}

// provision ensures the namespace and its scaffold exist and match the
// desired state (create-or-update — AC5 idempotent).
func (r *Reconciler) provision(ctx context.Context, teamObj *api.Team, nsName string) error {
	ns, err := r.ensureNamespace(ctx, teamObj, nsName)
	if err != nil {
		return err
	}

	objects := []client.Object{
		agentServiceAccount(nsName, teamObj),
		agentRole(nsName, teamObj),
		agentRoleBinding(nsName, teamObj),
		squadResourceQuota(nsName, teamObj),
		squadLimitRange(nsName, teamObj),
		defaultDenyNetworkPolicy(nsName, teamObj),
		allowDNSNetworkPolicy(nsName, teamObj, r.dnsNamespace()),
		allowControlPlaneNetworkPolicy(nsName, teamObj, r.controlPlaneNamespace()),
	}
	for _, obj := range objects {
		if err := ensureOwned(ctx, r.Client, obj, ns.UID); err != nil {
			return fmt.Errorf("ensure %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

// ensureNamespace creates the squad namespace if absent and returns it. A
// namespace that exists without the tenancy labels is foreign — the reconciler
// never adopts it (fail-closed condition, AC7 discipline). A managed
// namespace whose Pod Security labels drifted has them restored.
func (r *Reconciler) ensureNamespace(ctx context.Context, teamObj *api.Team, nsName string) (*corev1.Namespace, error) {
	var ns corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: nsName}, &ns)
	if apierrors.IsNotFound(err) {
		created := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   nsName,
				Labels: namespaceLabels(teamObj),
			},
		}
		if err := r.Create(ctx, created); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("namespace %s: lost create race", nsName)
			}
			return nil, fmt.Errorf("create squad namespace: %w", err)
		}
		return created, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get squad namespace: %w", err)
	}
	if ns.Labels[LabelTeam] != teamObj.Name || ns.Labels[LabelTenancy] != TenancySquad {
		setErrorCondition(ctx, r, teamObj, condNamespaceConflict,
			fmt.Sprintf("namespace %s exists but is not managed by this Team (missing %s/%s labels); refusing to adopt", nsName, LabelTeam, LabelTenancy))
		return nil, fmt.Errorf("team %s: namespace %s is not squad-managed", teamObj.Name, nsName)
	}
	// Restore drifted managed labels (PSA enforce set, team-namespace
	// routing label) without touching foreign labels.
	if mergeLabels(&ns, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Labels: namespaceLabels(teamObj)}}) {
		if err := r.Update(ctx, &ns); err != nil {
			return nil, fmt.Errorf("restore squad namespace labels: %w", err)
		}
	}
	return &ns, nil
}

// teardown implements the AC6 finalizer path: delete the squad namespace and
// clear the finalizer only after it is fully gone. While the namespace is
// Terminating (a contained resource's own finalizer wedging it), the Team
// finalizer stays and the NamespaceTerminating condition is kept fresh.
func (r *Reconciler) teardown(ctx context.Context, teamObj *api.Team) (ctrl.Result, error) {
	if !containsString(FinalizerTenancy, teamObj.Finalizers) {
		return ctrl.Result{}, nil
	}

	nsName := teamObj.Status.Namespace
	if nsName == "" {
		// Nothing was ever provisioned — just release the Team.
		return ctrl.Result{}, r.removeFinalizer(ctx, teamObj)
	}

	var ns corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: nsName}, &ns)
	switch {
	case apierrors.IsNotFound(err):
		// Namespace fully gone: teardown complete, finalizer may clear.
		clearConditions(teamObj, condNamespaceTerminating)
		return ctrl.Result{}, r.removeFinalizer(ctx, teamObj)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get namespace %s during teardown: %w", nsName, err)
	}

	// Never touch a namespace we do not manage (foreign label check again —
	// the name was recorded by us, but verify at delete time too).
	if ns.Labels[LabelTeam] != teamObj.Name {
		return ctrl.Result{}, r.removeFinalizer(ctx, teamObj)
	}

	if ns.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, &ns); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete squad namespace %s: %w", nsName, err)
		}
	}

	// Still present (deleting or about to): surface the stuck state and keep
	// requeueing — the finalizer must not clear while Terminating (AC6).
	setCondition(teamObj, condNamespaceTerminating, metav1.ConditionTrue,
		"NamespaceTerminating", fmt.Sprintf("namespace %s is terminating; tenancy finalizer held until it is gone", nsName))
	if err := r.Status().Update(ctx, teamObj); err != nil {
		return ctrl.Result{}, fmt.Errorf("update team status during teardown: %w", err)
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *Reconciler) removeFinalizer(ctx context.Context, teamObj *api.Team) error {
	patch := client.MergeFrom(teamObj.DeepCopy())
	teamObj.Finalizers = removeString(FinalizerTenancy, teamObj.Finalizers)
	if err := r.Patch(ctx, teamObj, patch); err != nil {
		return fmt.Errorf("remove tenancy finalizer: %w", err)
	}
	return nil
}

func (r *Reconciler) controlPlaneNamespace() string {
	if r.ControlPlaneNamespace == "" {
		return SystemNamespace
	}
	return r.ControlPlaneNamespace
}

// SetupWithManager registers the Team reconciler. The manager-managed client
// is adopted when one was not injected (tests inject a fake).
//
// Besides the Team itself, the controller WATCHES the scaffold kinds (and
// the namespace): deleting e.g. ksquad-default-deny must self-heal on the
// next event, not wait for the next Team write or the manager's 10h resync
// while the namespace runs wide open. Owns() cannot be used — it maps
// events via owner reference, and the scaffold's owner is the squad
// Namespace, not the Team — so each watch maps via the
// ksquad.io/team + ksquad.io/team-namespace labels instead.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	scaffoldEvents := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		return requestsForScaffoldObject(obj)
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.Team{}).
		Watches(&corev1.Namespace{}, scaffoldEvents).
		Watches(&corev1.ServiceAccount{}, scaffoldEvents).
		Watches(&rbacv1.Role{}, scaffoldEvents).
		Watches(&rbacv1.RoleBinding{}, scaffoldEvents).
		Watches(&corev1.ResourceQuota{}, scaffoldEvents).
		Watches(&corev1.LimitRange{}, scaffoldEvents).
		Watches(&networkingv1.NetworkPolicy{}, scaffoldEvents).
		Named("team").
		Complete(r)
}

// requestsForScaffoldObject maps a scaffold/namespace object event back to
// the reconcile request of the Team that owns it, via the routing labels.
// Objects without both labels (anything not provisioned by this
// controller) map to nothing.
func requestsForScaffoldObject(obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	team, okTeam := labels[LabelTeam]
	teamNS, okNS := labels[LabelTeamNamespace]
	if !okTeam || !okNS || team == "" || teamNS == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: team, Namespace: teamNS}}}
}

func (r *Reconciler) dnsNamespace() string {
	if r.DNSNamespace == "" {
		return "kube-system"
	}
	return r.DNSNamespace
}

// --- desired objects -------------------------------------------------------

// namespaceLabels is the desired label set of a squad namespace.
func namespaceLabels(teamObj *api.Team) map[string]string {
	return map[string]string{
		LabelTeam:              teamObj.Name,
		LabelTeamNamespace:     teamObj.Namespace,
		LabelTenancy:           TenancySquad,
		psaEnforceLabel:        "restricted",
		psaEnforceVersionLabel: "latest",
	}
}

// managedLabels are stamped on every object the reconciler owns.
func managedLabels(teamObj *api.Team) map[string]string {
	return map[string]string{
		LabelTeam:                      teamObj.Name,
		LabelTeamNamespace:             teamObj.Namespace,
		LabelTenancy:                   TenancySquad,
		"app.kubernetes.io/managed-by": "ksquad-operator",
	}
}

// agentServiceAccount is the least-privilege SA sandbox pods run as (story
// 4.2 AC3). API-token automounting is off: a sandbox never talks to the
// Kubernetes API, so it gets no credentials to.
func agentServiceAccount(ns string, teamObj *api.Team) *corev1.ServiceAccount {
	automount := false
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      AgentServiceAccount,
			Labels:    managedLabels(teamObj),
		},
		AutomountServiceAccountToken: &automount,
	}
}

// agentRole is the §12.1 least-privilege floor (story 4.1 AC2, the D2 crux):
//
//   - namespaced Role only — never a ClusterRole/ClusterRoleBinding;
//   - no wildcard resources/verbs, no nonResourceURLs, no cross-namespace reach;
//   - NO Secret access at all. A namespace-wide secrets get/list on the shared
//     squad SA would let any Run read every principal's BYO Secret, defeating
//     per-principal scoping (§9.4/story 4.5). Per-principal credentials are
//     injected as env/volumes at pod assembly (§7.3), never read via API. When
//     a future story needs enumerated get-by-name Secret reads, it escalates to
//     per-principal ServiceAccounts — not a namespace-wide grant here.
//   - NO pod or pod-log reads either: all Runs in the squad share this one
//     ServiceAccount, so a namespace-wide pods get/list/watch + pods/log get
//     would let any Run read every other Run's pod spec and log output —
//     cross-principal disclosure via a different object, and once
//     per-principal credentials land as env in pod specs (§7.3) it would leak
//     those too. The sandbox never talks to the Kubernetes API at all
//     (automountServiceAccountToken=false, no token to spend), so these
//     grants were latent risk with zero functional benefit.
//
// The floor is therefore EMPTY: the scaffold (Role + binding to the squad SA)
// exists so future grants are deliberate additions, but a sandbox pod needs
// no API permissions to run.
func agentRole(ns string, teamObj *api.Team) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      AgentServiceAccount,
			Labels:    managedLabels(teamObj),
		},
		Rules: []rbacv1.PolicyRule{},
	}
}

// agentRoleBinding binds the Role to the squad SA only (never
// system:authenticated, never a group).
func agentRoleBinding(ns string, teamObj *api.Team) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      AgentServiceAccount,
			Labels:    managedLabels(teamObj),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Namespace: ns,
			Name:      AgentServiceAccount,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     AgentServiceAccount,
		},
	}
}

// squadResourceQuota bounds the squad's aggregate footprint so one squad can
// never starve the cluster or another squad (story 4.1 AC3). Values are the
// documented platform defaults; the namespaceStrategy/Helm refinement layers
// on top of this floor.
func squadResourceQuota(ns string, teamObj *api.Team) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "ksquad-squad",
			Labels:    managedLabels(teamObj),
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourcePods:                   resource.MustParse("20"),
				corev1.ResourceRequestsCPU:            resource.MustParse("8"),
				corev1.ResourceRequestsMemory:         resource.MustParse("16Gi"),
				corev1.ResourceLimitsCPU:              resource.MustParse("16"),
				corev1.ResourceLimitsMemory:           resource.MustParse("32Gi"),
				corev1.ResourceRequestsStorage:        resource.MustParse("200Gi"),
				corev1.ResourcePersistentVolumeClaims: resource.MustParse("20"),
			},
		},
	}
}

// squadLimitRange bounds every container and PVC in the namespace (story 4.1
// AC3): a pod omitting requests/limits still lands bounded, and a PVC cannot
// be sized outside the squad's allowed range (the PVC type supports min/max
// only — no default request).
func squadLimitRange(ns string, teamObj *api.Team) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "ksquad-squad",
			Labels:    managedLabels(teamObj),
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Default: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("4Gi"),
					},
					DefaultRequest: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("250m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
					Max: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("4"),
						corev1.ResourceMemory: resource.MustParse("8Gi"),
					},
				},
				{
					Type: corev1.LimitTypePersistentVolumeClaim,
					Min: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
					Max: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("100Gi"),
					},
				},
			},
		},
	}
}

// defaultDenyNetworkPolicy is the §12.2 baseline (story 4.1 AC4): deny all
// ingress and egress for every pod in the namespace. The two companions below
// re-open exactly what a Run needs to function — a bare deny-all without them
// is a construction error, and further egress is added as explicit allowlists
// (story 4.6 / egressPolicyRef), never by removing this.
func defaultDenyNetworkPolicy(ns string, teamObj *api.Team) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "ksquad-default-deny",
			Labels:    managedLabels(teamObj),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

var (
	udpProtocol = corev1.ProtocolUDP
	tcpProtocol = corev1.ProtocolTCP
	dnsPort     = intstr.FromInt32(53)
)

// allowDNSNetworkPolicy re-opens cluster DNS (§12.2 companion, AC4).
func allowDNSNetworkPolicy(ns string, teamObj *api.Team, dnsNamespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "ksquad-allow-dns",
			Labels:    managedLabels(teamObj),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": dnsNamespace},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udpProtocol, Port: &dnsPort},
					{Protocol: &tcpProtocol, Port: &dnsPort},
				},
			}},
		},
	}
}

// allowControlPlaneNetworkPolicy re-opens egress to the control-plane
// namespace (§12.2 companion, AC4) — the Run must reach the
// apiserver/shim/memory service that live in ksquad-system.
func allowControlPlaneNetworkPolicy(ns string, teamObj *api.Team, controlPlaneNamespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "ksquad-allow-control-plane",
			Labels:    managedLabels(teamObj),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": controlPlaneNamespace},
					},
				}},
			}},
		},
	}
}

// --- helpers ---------------------------------------------------------------

// ensureOwned create-or-updates obj with an ownerReference to the squad
// namespace (namespace-scoped children cascade on namespace delete; the
// namespaced Team cannot own them directly — AC6 rationale). Updates fire
// only on drift, so steady-state reconciles write nothing (AC5). An empty
// nsUID (only possible pre-persist / in fixtures without a namespace UID)
// skips the owner-reference drift branch entirely: setNamespaceOwner is a
// no-op for it, and without the guard the reconcile would loop issuing
// no-op Updates forever.
func ensureOwned(ctx context.Context, c client.Client, desired client.Object, nsUID types.UID) error {
	existing := desired.DeepCopyObject().(client.Object)
	if err := c.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		setNamespaceOwner(desired, nsUID)
		return c.Create(ctx, desired)
	}

	changed := false
	if nsUID != "" && !hasNamespaceOwner(existing, nsUID) {
		setNamespaceOwner(existing, nsUID)
		changed = true
	}
	if mergeLabels(existing, desired) {
		changed = true
	}
	if mergePayload(existing, desired) {
		changed = true
	}
	if !changed {
		return nil
	}
	return c.Update(ctx, existing)
}

// mergeLabels restores drifted managed labels (e.g. a stripped
// ksquad.io/team label) without wiping foreign labels on the object, and
// reports whether anything drifted. The team labels are the tenancy filter
// the self-healing Watches key on — a stripped label must not stay
// stripped.
func mergeLabels(existing, desired client.Object) bool {
	desiredLabels := desired.GetLabels()
	if len(desiredLabels) == 0 {
		return false
	}
	existingLabels := existing.GetLabels()
	drifted := false
	for k, v := range desiredLabels {
		if existingLabels[k] != v {
			if existingLabels == nil {
				existingLabels = map[string]string{}
			}
			existingLabels[k] = v
			drifted = true
		}
	}
	if drifted {
		existing.SetLabels(existingLabels)
	}
	return drifted
}

// setNamespaceOwner stamps (replacing) the ownerReference to the namespace.
func setNamespaceOwner(obj client.Object, nsUID types.UID) {
	if nsUID == "" {
		return
	}
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "Namespace",
		Name:       obj.GetNamespace(),
		UID:        nsUID,
	}})
}

func hasNamespaceOwner(obj client.Object, nsUID types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "Namespace" && ref.UID == nsUID {
			return true
		}
	}
	return false
}

// mergePayload copies the reconciler-owned payload fields from desired onto
// existing and reports whether anything drifted.
func mergePayload(existing, desired client.Object) bool {
	drifted := false
	switch e := existing.(type) {
	case *corev1.ServiceAccount:
		d := desired.(*corev1.ServiceAccount)
		if !apiequality.Semantic.DeepEqual(e.AutomountServiceAccountToken, d.AutomountServiceAccountToken) {
			e.AutomountServiceAccountToken = d.AutomountServiceAccountToken
			drifted = true
		}
	case *rbacv1.Role:
		d := desired.(*rbacv1.Role)
		if !apiequality.Semantic.DeepEqual(e.Rules, d.Rules) {
			e.Rules = d.Rules
			drifted = true
		}
	case *rbacv1.RoleBinding:
		d := desired.(*rbacv1.RoleBinding)
		if !apiequality.Semantic.DeepEqual(e.Subjects, d.Subjects) || !apiequality.Semantic.DeepEqual(e.RoleRef, d.RoleRef) {
			e.Subjects = d.Subjects
			e.RoleRef = d.RoleRef
			drifted = true
		}
	case *corev1.ResourceQuota:
		d := desired.(*corev1.ResourceQuota)
		if !apiequality.Semantic.DeepEqual(e.Spec, d.Spec) {
			e.Spec = d.Spec
			drifted = true
		}
	case *corev1.LimitRange:
		d := desired.(*corev1.LimitRange)
		if !apiequality.Semantic.DeepEqual(e.Spec, d.Spec) {
			e.Spec = d.Spec
			drifted = true
		}
	case *networkingv1.NetworkPolicy:
		d := desired.(*networkingv1.NetworkPolicy)
		if !apiequality.Semantic.DeepEqual(e.Spec, d.Spec) {
			e.Spec = d.Spec
			drifted = true
		}
	}
	return drifted
}

func setCondition(obj *api.Team, condType string, status metav1.ConditionStatus, reason, message string) bool {
	return meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: obj.Generation,
	})
}

func clearConditions(obj *api.Team, condTypes ...string) {
	for _, t := range condTypes {
		meta.RemoveStatusCondition(&obj.Status.Conditions, t)
	}
}

// setErrorCondition records a fail-closed condition on the Team status. A
// status-update failure is logged, not fatal — the reconcile error already
// requeues, and a wedged status write must not mask the original cause.
func setErrorCondition(ctx context.Context, r *Reconciler, obj *api.Team, condType, message string) {
	setCondition(obj, condType, metav1.ConditionTrue, "FailClosed", message)
	if err := r.Status().Update(ctx, obj); err != nil {
		log.FromContext(ctx).Error(err, "record fail-closed condition", "condition", condType)
	}
}

func containsString(s string, slice []string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func removeString(s string, slice []string) []string {
	out := make([]string, 0, len(slice))
	for _, v := range slice {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
