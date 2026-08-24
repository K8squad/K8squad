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

// workspace.go — workspace PVC management for agent execution (ISI-2880).
// This controller creates and manages persistent volume claims for agent
// workspaces, providing isolated storage for each Run execution.
package workspace

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

const (
	// WorkspacePVCSize is the default size for agent workspace PVCs
	WorkspacePVCSize = "10Gi"
	
	// WorkspaceStorageClass is the default storage class for workspace PVCs
	// This should be configurable via Helm chart values
	WorkspaceStorageClass = "standard"
	
	// LabelWorkspace identifies PVCs managed by this controller
	LabelWorkspace = "k8squad.io/workspace"
	
	// LabelRun identifies the Run that owns this workspace
	LabelRun = "k8squad.io/run"
)

// Manager manages PVC lifecycle for agent workspaces
type Manager struct {
	client client.Client
}

// NewManager creates a new workspace manager
func NewManager(kubeClient client.Client) *Manager {
	return &Manager{
		client: kubeClient,
	}
}

// Reconcile ensures a workspace PVC exists for each Run. PVC teardown is handled
// by the owner reference set in createWorkspacePVC, so a deleted/absent Run is a
// no-op here.
func (wm *Manager) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	run := &ksquadv1alpha1.Run{}
	if err := wm.client.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !run.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if _, err := wm.EnsureWorkspace(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// EnsureWorkspace ensures that a workspace PVC exists for the given Run.
// If the PVC doesn't exist, it creates one. If it exists, it returns the reference.
func (wm *Manager) EnsureWorkspace(ctx context.Context, run *ksquadv1alpha1.Run) (*corev1.PersistentVolumeClaim, error) {
	logger := log.FromContext(ctx)
	
	// Generate the PVC name for this Run
	pvcName := fmt.Sprintf("workspace-%s", run.Name)
	
	// Check if PVC already exists
	existingPVC := &corev1.PersistentVolumeClaim{}
	err := wm.client.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: run.Namespace}, existingPVC)
	
	if err == nil {
		// PVC already exists, return it
		logger.Info("Workspace PVC already exists", "pvc", pvcName, "run", run.Name)
		return existingPVC, nil
	}
	
	if !errors.IsNotFound(err) {
		// Other error occurred
		return nil, fmt.Errorf("failed to check existing PVC %s: %w", pvcName, err)
	}
	
	// PVC doesn't exist, create it
	pvc := wm.createWorkspacePVC(run, pvcName)
	
	if err := wm.client.Create(ctx, pvc); err != nil {
		return nil, fmt.Errorf("failed to create workspace PVC %s: %w", pvcName, err)
	}
	
	logger.Info("Created workspace PVC", "pvc", pvcName, "run", run.Name)
	return pvc, nil
}

// GetWorkspace returns the workspace PVC for the given Run
func (wm *Manager) GetWorkspace(ctx context.Context, run *ksquadv1alpha1.Run) (*corev1.PersistentVolumeClaim, error) {
	pvcName := fmt.Sprintf("workspace-%s", run.Name)
	
	pvc := &corev1.PersistentVolumeClaim{}
	err := wm.client.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: run.Namespace}, pvc)
	
	if err != nil {
		return nil, fmt.Errorf("failed to get workspace PVC %s: %w", pvcName, err)
	}
	
	return pvc, nil
}

// DeleteWorkspace deletes the workspace PVC for the given Run
func (wm *Manager) DeleteWorkspace(ctx context.Context, run *ksquadv1alpha1.Run) error {
	logger := log.FromContext(ctx)
	
	pvcName := fmt.Sprintf("workspace-%s", run.Name)
	
	pvc := &corev1.PersistentVolumeClaim{}
	err := wm.client.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: run.Namespace}, pvc)
	
	if err != nil {
		if errors.IsNotFound(err) {
			// PVC doesn't exist, nothing to do
			logger.Info("Workspace PVC already deleted", "pvc", pvcName, "run", run.Name)
			return nil
		}
		return fmt.Errorf("failed to get workspace PVC %s for deletion: %w", pvcName, err)
	}
	
	// Delete the PVC
	deletePolicy := metav1.DeletePropagationForeground
	if err := wm.client.Delete(ctx, pvc, client.PropagationPolicy(deletePolicy)); err != nil {
		return fmt.Errorf("failed to delete workspace PVC %s: %w", pvcName, err)
	}
	
	logger.Info("Deleted workspace PVC", "pvc", pvcName, "run", run.Name)
	return nil
}

// createWorkspacePVC creates a workspace PVC for the given Run
func (wm *Manager) createWorkspacePVC(run *ksquadv1alpha1.Run, pvcName string) *corev1.PersistentVolumeClaim {
	// ponytail: RunSpec.SandboxPolicy carries no per-Run workspace sizing/class
	// fields (api/v1alpha1/run_types.go), so we use the package defaults.
	// Upgrade path: add a WorkspacePVC *PVCSpec to SandboxPolicy (mirror
	// ProjectSpec.WorkspacePVC) and read Size/Class from it here.
	storageClass := WorkspaceStorageClass
	pvcSize := WorkspacePVCSize
	
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: run.Namespace,
			Labels: map[string]string{
				LabelWorkspace: "true",
				LabelRun:       run.Name,
			},
			Annotations: map[string]string{
				"k8squad.io/run":        run.Name,
				"k8squad.io/workspace":  "agent-workspace",
				"k8squad.io/created-by": "workspace-controller",
			},
			// Owner reference for automatic cleanup when Run is deleted
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         ksquadv1alpha1.GroupVersion.String(),
					Kind:               "Run",
					Name:               run.Name,
					UID:                run.UID,
					Controller:         ptrTo(true),
					BlockOwnerDeletion: ptrTo(true),
				},
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce, // One node can mount it read-write
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					"storage": resource.MustParse(pvcSize),
				},
			},
			StorageClassName: &storageClass,
		},
	}
	
	return pvc
}

// VolumeMount returns a volume mount specification for the workspace PVC
func VolumeMount(runName string) corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      "workspace",
		MountPath: "/workspace",
		ReadOnly:  false,
	}
}

// Volume returns a volume specification for the workspace PVC
func Volume(runName string) corev1.Volume {
	return corev1.Volume{
		Name: "workspace",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: fmt.Sprintf("workspace-%s", runName),
			},
		},
	}
}

// ptrTo returns a pointer to the given value
func ptrTo[T any](v T) *T {
	return &v
}

// IsWorkspaceOwned checks if the given PVC is owned by a workspace
func IsWorkspaceOwned(pvc *corev1.PersistentVolumeClaim) bool {
	if pvc == nil {
		return false
	}
	
	if pvc.Labels == nil {
		return false
	}
	
	_, isWorkspace := pvc.Labels[LabelWorkspace]
	_, isRun := pvc.Labels[LabelRun]
	
	return isWorkspace && isRun
}

// GetRunForWorkspace returns the Run that owns the given PVC
func GetRunForWorkspace(pvc *corev1.PersistentVolumeClaim) (string, bool) {
	if pvc == nil || pvc.Labels == nil {
		return "", false
	}
	
	runName, ok := pvc.Labels[LabelRun]
	return runName, ok
}