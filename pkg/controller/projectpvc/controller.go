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
// the Project's namespace, owner-referenced to the Project so claim lifetime
// follows the Project. The team's agent runtime pods mount this claim
// shared (the warm-pool Boot path, pkg/warmpool/kube.go), which is what
// lets files written by one agent be read by another and survive pod
// teardown.
//
// Fail-closed storage-class discipline (api/v1alpha1 PVCSpec contract,
// ISI-3745): the class comes from spec.workspacePVC.class, else the
// operator's Helm-injected KSQUAD_WORKSPACE_STORAGE_CLASS — NEVER the
// cluster default. When neither resolves, no PVC is created and the
// Project's WorkspaceReady condition reports why.
//
// Data-safety posture: removing spec.workspacePVC (or shrinking the
// requested size) NEVER deletes or shrinks the claim — PVC teardown follows
// Project deletion via the owner reference, and only there.
package projectpvc

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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

// The cache watches Projects and (via Owns) their PVCs cluster-wide, so the
// RBAC set below is exactly what the reconciler exercises — PVC delete is
// absent on purpose: claim teardown is owner-reference GC only.
//+kubebuilder:rbac:groups=ksquad.io,resources=projects,verbs=get;list;watch
//+kubebuilder:rbac:groups=ksquad.io,resources=projects/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch

// Reconcile converges the Project's workspace PVC. A missing/deleting
// Project is a no-op (owner-ref GC reaps the claim); a Project without
// spec.workspacePVC is a no-op (spec removal never destroys data).
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

	name := workspace.ProjectPVCName(project.Name)
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: project.Namespace}, existing)
	switch {
	case errors.IsNotFound(err):
		return ctrl.Result{}, r.create(ctx, project, spec, class, name)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("projectpvc: get PVC %s/%s: %w", project.Namespace, name, err)
	default:
		return ctrl.Result{}, r.converge(ctx, project, spec, class, existing)
	}
}

// create stamps the initial claim and reports it. The claim may stay
// Pending (WaitForFirstConsumer classes bind at first mount) — existence is
// the readiness bar here; the mount path surfaces a bind failure on its own.
func (r *Reconciler) create(ctx context.Context, project *api.Project, spec *api.PVCSpec, class, name string) error {
	logger := log.FromContext(ctx)
	pvc := buildProjectPVC(project, spec, class, name)
	if err := r.Create(ctx, pvc); err != nil {
		if errors.IsAlreadyExists(err) {
			// Lost the create race with a concurrent reconcile; requeue
			// through the converge path rather than erroring.
			return nil
		}
		return fmt.Errorf("projectpvc: create PVC %s/%s: %w", project.Namespace, name, err)
	}
	logger.Info("created project workspace PVC", "pvc", name, "project", project.Name, "class", class)
	return r.patchCondition(ctx, project, metav1.Condition{
		Type:    ConditionWorkspaceReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonProvisioned,
		Message: fmt.Sprintf("workspace PVC %s provisioned (class %s)", name, class),
	})
}

// converge reconciles an existing claim against the spec under PVC
// immutability rules: class and access modes are fixed at creation (a
// mismatch is reported, never mutated — recreating a bound claim would
// destroy data); only size EXPANSION is applied in place.
func (r *Reconciler) converge(ctx context.Context, project *api.Project, spec *api.PVCSpec, class string, pvc *corev1.PersistentVolumeClaim) error {
	if !ownedByProject(pvc, project) {
		// A claim we did not create squats on the deterministic name. Never
		// adopt or mutate a stranger's data — report and stop.
		return r.patchCondition(ctx, project, metav1.Condition{
			Type:    ConditionWorkspaceReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonNameConflict,
			Message: fmt.Sprintf("PVC %s exists but is not owned by Project %s; refusing to adopt or mutate it", pvc.Name, project.Name),
		})
	}

	if got := pvc.Spec.StorageClassName; got == nil || *got != class {
		return r.patchCondition(ctx, project, metav1.Condition{
			Type:    ConditionWorkspaceReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonSpecConflict,
			Message: fmt.Sprintf("PVC %s is bound to class %q but the spec resolves %q; storage class is immutable — migrate the claim manually", pvc.Name, storageClassOf(pvc), class),
		})
	}
	if !apiequality.Semantic.DeepEqual(pvc.Spec.AccessModes, spec.EffectiveAccessModes()) {
		return r.patchCondition(ctx, project, metav1.Condition{
			Type:    ConditionWorkspaceReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonSpecConflict,
			Message: fmt.Sprintf("PVC %s has access modes %v but the spec wants %v; access modes are immutable — migrate the claim manually", pvc.Name, pvc.Spec.AccessModes, spec.EffectiveAccessModes()),
		})
	}

	current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if spec.Size.Cmp(current) > 0 {
		patched := pvc.DeepCopy()
		patched.Spec.Resources.Requests[corev1.ResourceStorage] = spec.Size
		if err := r.Patch(ctx, patched, client.MergeFrom(pvc)); err != nil {
			return fmt.Errorf("projectpvc: expand PVC %s/%s to %s: %w", pvc.Namespace, pvc.Name, spec.Size.String(), err)
		}
		log.FromContext(ctx).Info("expanded project workspace PVC", "pvc", pvc.Name, "from", current.String(), "to", spec.Size.String())
	}
	// A requested SHRINK is silently clamped to the current size: PVC
	// contraction is impossible and data-preserving by design.

	return r.patchCondition(ctx, project, metav1.Condition{
		Type:    ConditionWorkspaceReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonProvisioned,
		Message: fmt.Sprintf("workspace PVC %s provisioned (class %s, phase %s)", pvc.Name, class, pvc.Status.Phase),
	})
}

// buildProjectPVC renders the desired claim: labels identify it as the
// Project's workspace (LabelWorkspace without LabelRun — distinct from the
// per-Run claims IsWorkspaceOwned matches), the controller owner reference
// ties its lifetime to the Project.
func buildProjectPVC(project *api.Project, spec *api.PVCSpec, class, name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: project.Namespace,
			Labels: map[string]string{
				workspace.LabelWorkspace: "true",
				workspace.LabelProject:   project.Name,
			},
			Annotations: map[string]string{
				"k8squad.io/project":    project.Name,
				"k8squad.io/created-by": "project-pvc-controller",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         api.GroupVersion.String(),
					Kind:               "Project",
					Name:               project.Name,
					UID:                project.UID,
					Controller:         ptrTo(true),
					BlockOwnerDeletion: ptrTo(true),
				},
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

// ownedByProject reports whether the claim carries this Project as its
// controller owner reference.
func ownedByProject(pvc *corev1.PersistentVolumeClaim, project *api.Project) bool {
	for _, ref := range pvc.OwnerReferences {
		if ref.Kind == "Project" && ref.Name == project.Name && ref.UID == project.UID &&
			ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
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
// status writes from hot-looping this loop, while the Owns watch still
// requeues on claim phase drift.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("project-pvc").
		For(&api.Project{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}

// ptrTo returns a pointer to the given value.
func ptrTo[T any](v T) *T {
	return &v
}
