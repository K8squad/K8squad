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

// Package projectpvc provisions the per-Project workspace PVC (ISI-4127,
// arch §5.1/§9.4, story 4.3): for every Project carrying
// spec.workspacePVC, exactly one claim named by workspace.ProjectPVCName in
// EACH consuming team's sandbox namespace — the namespace the team's agent
// runtime pods boot in (ADR-044 step 9), which is the only place a mount
// can resolve: Kubernetes PVCs are namespace-scoped and cross-namespace
// mounts do not exist (ISI-4302 — the claim previously landed in the
// Project's own namespace, so sandbox pods hit FailedScheduling
// "persistentvolumeclaim not found" and WaitForFirstConsumer classes like
// local-path never saw a consumer, leaving the mis-placed claim Pending
// forever). Consumption is resolved through Team.spec.projects refs.
//
// Fail-closed storage-class discipline (api/v1alpha1 PVCSpec contract,
// ISI-3745): the class comes from spec.workspacePVC.class, else the
// operator's Helm-injected KSQUAD_WORKSPACE_STORAGE_CLASS — NEVER the
// cluster default. When neither resolves, no PVC is created and the
// Project's WorkspaceReady condition reports why.
//
// Data-safety posture: removing spec.workspacePVC (or shrinking the
// requested size) NEVER deletes or shrinks a claim; nor does removing the
// Project from a Team. Team-namespace claims carry no owner reference
// (cross-namespace owner refs are invalid), so their lifetime is bounded
// by the team namespace itself — the team reconciler reaps the namespace
// (and everything in it) on Team deletion.
package projectpvc

import (
	"context"
	"fmt"
	"os"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/workspace"
)

const (
	// ConditionWorkspaceReady reports the per-Project workspace claim on
	// Project.status.conditions. It coexists with the repo-sync
	// reconciler's SyncReady condition: both writers touch only their own
	// condition type through MergeFrom status patches (the reposync
	// pattern, pkg/controller/reposync).
	ConditionWorkspaceReady = "WorkspaceReady"

	reasonProvisioned            = "Provisioned"
	reasonStorageClassUnresolved = "StorageClassUnresolved"
	reasonSpecConflict           = "SpecConflict"
	reasonNameConflict           = "NameConflict"
	reasonNoConsumingTeam        = "NoConsumingTeam"
	reasonTeamNamespacePending   = "TeamNamespacePending"

	// createdByAnnotation stamps team-namespace claims as ours: they cannot
	// carry a cross-namespace owner reference to the Project, so identity
	// is the (LabelProject label, created-by annotation) pair — same
	// never-adopt-a-stranger discipline the owner ref used to provide.
	createdByAnnotation = "k8squad.io/created-by"
	createdByProjectPVC = "project-pvc-controller"
)

// Reconciler ensures the per-Project workspace PVC for every Project with
// spec.workspacePVC set.
type Reconciler struct {
	client.Client

	// storageClass is the operator-level fallback class injected by the
	// Helm storage contract (KSQUAD_WORKSPACE_STORAGE_CLASS, ISI-3745). It
	// is resolved once at construction; there is deliberately NO package
	// constant fallback here — the PVCSpec contract fails closed when
	// neither spec nor environment names a class.
	storageClass string
}

// NewReconciler constructs the reconciler over the manager's client.
func NewReconciler(kubeClient client.Client) *Reconciler {
	return &Reconciler{
		Client:       kubeClient,
		storageClass: os.Getenv(workspace.EnvWorkspaceStorageClass),
	}
}

// The cache watches Projects, Teams (consumption resolution, ISI-4302) and
// (via the labeled map fn) the project workspace claims cluster-wide, so
// the RBAC set below is exactly what the reconciler exercises — PVC delete
// is absent on purpose: claims are never destroyed by this loop.
//+kubebuilder:rbac:groups=ksquad.io,resources=projects,verbs=get;list;watch
//+kubebuilder:rbac:groups=ksquad.io,resources=projects/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ksquad.io,resources=teams,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch

// Reconcile converges the Project's workspace PVC in every consuming
// team's sandbox namespace. A missing/deleting Project is a no-op; a
// Project without spec.workspacePVC is a no-op (spec removal never
// destroys data).
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	project := &api.Project{}
	if err := r.Get(ctx, req.NamespacedName, project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !project.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	spec := project.Spec.WorkspacePVC
	if spec == nil {
		return ctrl.Result{}, nil
	}

	class := spec.Class
	if class == "" {
		class = r.storageClass
	}
	if class == "" {
		// Fail closed (PVCSpec contract): no class from spec OR the Helm
		// storage contract means no PVC — a silent cluster-default bind is
		// the misconfiguration this contract exists to prevent.
		return ctrl.Result{}, r.patchCondition(ctx, project, metav1.Condition{
			Type:    ConditionWorkspaceReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonStorageClassUnresolved,
			Message: "spec.workspacePVC.class is empty and the operator has no KSQUAD_WORKSPACE_STORAGE_CLASS fallback; refusing to bind the cluster default storage class",
		})
	}

	targets, referencing, err := r.consumingNamespaces(ctx, project)
	if err != nil {
		return ctrl.Result{}, err
	}
	if referencing == 0 {
		// The claim is only mountable from a sandbox namespace; with no
		// consumer there is nowhere to place it (a claim in the Project's
		// own namespace can never bind under WaitForFirstConsumer classes
		// and would sit Pending forever — ISI-4302).
		return ctrl.Result{}, r.patchCondition(ctx, project, metav1.Condition{
			Type:    ConditionWorkspaceReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonNoConsumingTeam,
			Message: "no Team lists this Project in spec.projects; add it to a Team so the workspace claim can be provisioned in the team's sandbox namespace where agent pods mount it",
		})
	}
	if len(targets) == 0 {
		// Referenced, but the team(s) have not resolved a sandbox namespace
		// yet. The Team watch requeues this Project when status.namespace
		// lands — no error, no hot loop.
		return ctrl.Result{}, r.patchCondition(ctx, project, metav1.Condition{
			Type:    ConditionWorkspaceReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonTeamNamespacePending,
			Message: "referencing Team(s) have no status.namespace yet; waiting for the team reconciler to provision the sandbox namespace",
		})
	}

	name := workspace.ProjectPVCName(project.Name)
	var report *metav1.Condition
	ensured := make([]string, 0, len(targets))
	for _, ns := range targets {
		cond, err := r.ensureClaim(ctx, project, spec, class, name, ns)
		if err != nil {
			return ctrl.Result{}, err
		}
		if cond != nil {
			if report == nil {
				report = cond
			}
			continue
		}
		ensured = append(ensured, ns)
	}
	if report != nil {
		return ctrl.Result{}, r.patchCondition(ctx, project, *report)
	}
	return ctrl.Result{}, r.patchCondition(ctx, project, metav1.Condition{
		Type:    ConditionWorkspaceReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonProvisioned,
		Message: fmt.Sprintf("workspace PVC %s ensured in team namespace(s) %v (class %s)", name, ensured, class),
	})
}

// consumingNamespaces resolves the sandbox namespaces of every Team whose
// spec.projects references the Project (ObjectRef semantics: empty
// namespace = the Team's own). Returns the deduplicated, sorted list of
// RESOLVED target namespaces plus the count of referencing Teams — the
// caller distinguishes "no consumer at all" from "consumer not ready yet".
func (r *Reconciler) consumingNamespaces(ctx context.Context, project *api.Project) ([]string, int, error) {
	var teams api.TeamList
	if err := r.List(ctx, &teams); err != nil {
		return nil, 0, fmt.Errorf("projectpvc: list teams: %w", err)
	}
	seen := make(map[string]struct{})
	targets := make([]string, 0, 1)
	referencing := 0
	for i := range teams.Items {
		team := &teams.Items[i]
		matches := false
		for _, ref := range team.Spec.Projects {
			ns := ref.Namespace
			if ns == "" {
				ns = team.Namespace
			}
			if ns == project.Namespace && ref.Name == project.Name {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		referencing++
		if sandboxNS := team.Status.Namespace; sandboxNS != "" {
			if _, dup := seen[sandboxNS]; !dup {
				seen[sandboxNS] = struct{}{}
				targets = append(targets, sandboxNS)
			}
		}
	}
	sort.Strings(targets)
	return targets, referencing, nil
}

// ensureClaim converges the claim at (ns, name): creates it when absent,
// expands it when the spec grew, and returns a Condition (nil when
// healthy) for the reportable states — a stranger squats the
// deterministic name, or the class/access modes drifted under PVC
// immutability. Hard API failures return errors; reportable states never
// requeue on their own.
func (r *Reconciler) ensureClaim(ctx context.Context, project *api.Project, spec *api.PVCSpec, class, name, ns string) (*metav1.Condition, error) {
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing)
	switch {
	case errors.IsNotFound(err):
		return nil, r.create(ctx, project, spec, class, name, ns)
	case err != nil:
		return nil, fmt.Errorf("projectpvc: get PVC %s/%s: %w", ns, name, err)
	default:
		return r.converge(ctx, project, spec, class, existing)
	}
}

// create stamps the initial claim; the caller's aggregate condition
// reports the ensured namespace set. The claim may stay Pending until
// first mount (WaitForFirstConsumer classes bind at the consuming pod);
// existence is the readiness bar here, and the mount path now shares the
// claim's namespace (ISI-4302).
func (r *Reconciler) create(ctx context.Context, project *api.Project, spec *api.PVCSpec, class, name, ns string) error {
	logger := log.FromContext(ctx)
	pvc := buildProjectPVC(project, spec, class, name, ns)
	if err := r.Create(ctx, pvc); err != nil {
		if errors.IsAlreadyExists(err) {
			// Lost the create race with a concurrent reconcile; requeue
			// through the converge path rather than erroring.
			return nil
		}
		return fmt.Errorf("projectpvc: create PVC %s/%s: %w", ns, name, err)
	}
	logger.Info("created project workspace PVC", "pvc", name, "namespace", ns, "project", project.Name, "class", class)
	return nil
}

// converge reconciles an existing claim against the spec under PVC
// immutability rules: class and access modes are fixed at creation (a
// mismatch is reported, never mutated — recreating a bound claim would
// destroy data); only size EXPANSION is applied in place.
func (r *Reconciler) converge(ctx context.Context, project *api.Project, spec *api.PVCSpec, class string, pvc *corev1.PersistentVolumeClaim) (*metav1.Condition, error) {
	if !isOurClaim(pvc, project) {
		// A claim we did not create squats on the deterministic name. Never
		// adopt or mutate a stranger's data — report and stop.
		cond := metav1.Condition{
			Type:    ConditionWorkspaceReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonNameConflict,
			Message: fmt.Sprintf("PVC %s/%s exists but was not provisioned for Project %s; refusing to adopt or mutate it", pvc.Namespace, pvc.Name, project.Name),
		}
		return &cond, nil
	}

	if got := pvc.Spec.StorageClassName; got == nil || *got != class {
		cond := metav1.Condition{
			Type:    ConditionWorkspaceReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonSpecConflict,
			Message: fmt.Sprintf("PVC %s/%s is bound to class %q but the spec resolves %q; storage class is immutable — migrate the claim manually", pvc.Namespace, pvc.Name, storageClassOf(pvc), class),
		}
		return &cond, nil
	}
	if !apiequality.Semantic.DeepEqual(pvc.Spec.AccessModes, spec.EffectiveAccessModes()) {
		cond := metav1.Condition{
			Type:    ConditionWorkspaceReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonSpecConflict,
			Message: fmt.Sprintf("PVC %s/%s has access modes %v but the spec wants %v; access modes are immutable — migrate the claim manually", pvc.Namespace, pvc.Name, pvc.Spec.AccessModes, spec.EffectiveAccessModes()),
		}
		return &cond, nil
	}

	current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if spec.Size.Cmp(current) > 0 {
		patched := pvc.DeepCopy()
		patched.Spec.Resources.Requests[corev1.ResourceStorage] = spec.Size
		if err := r.Patch(ctx, patched, client.MergeFrom(pvc)); err != nil {
			return nil, fmt.Errorf("projectpvc: expand PVC %s/%s to %s: %w", pvc.Namespace, pvc.Name, spec.Size.String(), err)
		}
		log.FromContext(ctx).Info("expanded project workspace PVC", "pvc", pvc.Name, "namespace", pvc.Namespace, "from", current.String(), "to", spec.Size.String())
	}
	// A requested SHRINK is silently clamped to the current size: PVC
	// contraction is impossible and data-preserving by design.

	return nil, nil
}

// buildProjectPVC renders the desired claim for a team sandbox namespace:
// labels identify it as the Project's workspace (LabelWorkspace without
// LabelRun — distinct from the per-Run claims IsWorkspaceOwned matches),
// and the created-by annotation carries claim identity — an owner
// reference is impossible here (cross-namespace owner refs are rejected
// by the API server), so lifetime rides the team namespace instead.
func buildProjectPVC(project *api.Project, spec *api.PVCSpec, class, name, ns string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				workspace.LabelWorkspace: "true",
				workspace.LabelProject:   project.Name,
			},
			Annotations: map[string]string{
				"k8squad.io/project":    project.Name,
				"k8squad.io/created-by": createdByProjectPVC,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: spec.EffectiveAccessModes(),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: spec.Size,
				},
			},
			StorageClassName: &class,
		},
	}
}

// isOurClaim reports whether the claim was provisioned for this Project:
// the LabelProject label plus the created-by annotation. Team-namespace
// claims cannot owner-reference the Project (cross-namespace owner refs
// are invalid), so this pair is the identity — and a claim failing it is
// a stranger we never adopt.
func isOurClaim(pvc *corev1.PersistentVolumeClaim, project *api.Project) bool {
	return pvc.Labels[workspace.LabelProject] == project.Name &&
		pvc.Annotations[createdByAnnotation] == createdByProjectPVC
}

func storageClassOf(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName == nil {
		return ""
	}
	return *pvc.Spec.StorageClassName
}

// patchCondition merges the WorkspaceReady condition onto Project status,
// preserving unrelated conditions and the reposync-owned status slice
// (DeepCopy first, per-type LastTransitionTime, DeepEqual guard, MergeFrom
// patch — the reposync patchStatus discipline).
func (r *Reconciler) patchCondition(ctx context.Context, project *api.Project, condition metav1.Condition) error {
	next := project.DeepCopy()
	condition.LastTransitionTime = lastTransition(next.Status.Conditions, condition.Type, condition.Status)
	apimeta.SetStatusCondition(&next.Status.Conditions, condition)
	if apiequality.Semantic.DeepEqual(project.Status, next.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, next, client.MergeFrom(project)); err != nil {
		return fmt.Errorf("projectpvc: patch project status %s/%s: %w", project.Namespace, project.Name, err)
	}
	return nil
}

func lastTransition(conditions []metav1.Condition, condType string, status metav1.ConditionStatus) metav1.Time {
	for _, c := range conditions {
		if c.Type == condType && c.Status == status {
			return c.LastTransitionTime
		}
	}
	return metav1.Now()
}

// SetupWithManager registers the reconciler. The name MUST differ from the
// repo-sync reconciler's "project" (controller-runtime rejects duplicate
// controller names); the generation-change predicate keeps reposync's
// status writes from hot-looping this loop. Team events requeue every
// Project the Team consumes — on spec.projects changes (generation) and on
// status.namespace resolution (a status write, invisible to the generation
// predicate). Project claims are watched by label instead of Owns():
// team-namespace claims cannot owner-reference the Project (cross-ns
// owner refs are invalid), so the label map fn is the one watch covering
// both phase drift and in-place expansion requeues.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("project-pvc").
		For(&api.Project{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&api.Team{}, handler.EnqueueRequestsFromMapFunc(r.projectsForTeam), builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldTeam, oldOK := e.ObjectOld.(*api.Team)
				newTeam, newOK := e.ObjectNew.(*api.Team)
				if !oldOK || !newOK {
					return true
				}
				return oldTeam.Generation != newTeam.Generation ||
					oldTeam.Status.Namespace != newTeam.Status.Namespace
			},
		})).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.projectsForClaim)).
		Complete(r)
}

// projectsForTeam maps a Team event onto the Projects it consumes
// (spec.projects; ObjectRef semantics — empty namespace = the Team's own).
func (r *Reconciler) projectsForTeam(_ context.Context, obj client.Object) []reconcile.Request {
	team, ok := obj.(*api.Team)
	if !ok {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(team.Spec.Projects))
	for _, ref := range team.Spec.Projects {
		ns := ref.Namespace
		if ns == "" {
			ns = team.Namespace
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: ref.Name}})
	}
	return reqs
}

// projectsForClaim maps a project workspace claim event onto the Projects
// it may belong to. Per-Run claims carry LabelRun and no LabelProject, so
// they never match; the label carries only the Project name (the claim
// lives in a team namespace), so every same-named Project with a workspace
// spec is enqueued — over-matching is a cheap idempotent reconcile, never
// a missed one.
func (r *Reconciler) projectsForClaim(ctx context.Context, obj client.Object) []reconcile.Request {
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return nil
	}
	if pvc.Labels[workspace.LabelWorkspace] != "true" {
		return nil
	}
	name := pvc.Labels[workspace.LabelProject]
	if name == "" {
		return nil
	}
	var projects api.ProjectList
	if err := r.List(ctx, &projects); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, 1)
	for i := range projects.Items {
		if projects.Items[i].Name == name && projects.Items[i].Spec.WorkspacePVC != nil {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: projects.Items[i].Namespace, Name: projects.Items[i].Name},
			})
		}
	}
	return reqs
}
