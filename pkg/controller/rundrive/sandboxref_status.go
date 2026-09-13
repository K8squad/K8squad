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

package rundrive

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// RunStatusSandboxWriter implements coord.SandboxRefObserver (M1.2): it
// patches Run.status.sandboxRef the moment the warm pool binds a sandbox, so
// the console/E2E surface (which the run status projector only preserves,
// never writes) learns which pod serves the Run. It lives in the rundrive
// layer — not pkg/coord — because it needs the k8s client to load the Run CR
// by uid, exactly like the SecretCredentialWriter it runs beside (ADR-0007
// rev.2's layering pin).
type RunStatusSandboxWriter struct {
	client client.Client
}

// NewRunStatusSandboxWriter binds the observer over the manager client. A nil
// client yields nil (ref-silent mode), mirroring NewSecretCredentialWriter.
func NewRunStatusSandboxWriter(c client.Client) *RunStatusSandboxWriter {
	if c == nil {
		return nil
	}
	return &RunStatusSandboxWriter{client: c}
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;patch

// ObserveSandboxRef patches Run.status.sandboxRef for the Run whose uid is
// runID (merge patch, only-when-different: idempotent by construction, and a
// re-drive of an already-bound Run re-observes the same ref harmlessly). The
// sandbox pod runs in the Run's own namespace (SpecClassifier's ADR-044
// step-9 key), so the ref's namespace resolves from the Run itself.
//
// Before touching status it adopts the sandbox pod under the Run CR via an
// ownerReference (ISI-4366): warm-pool pods are booted with NO owner, so a Run
// deleted while non-terminal (the terminal release path never runs) strands
// the pod with no owner for Kubernetes GC to reap. Setting the Run as the
// controller owner at bind guarantees the pod is garbage-collected on Run
// deletion even when the terminal projection or Release path is skipped —
// defense-in-depth over ISI-4195's resync. Adoption runs on every observe
// (before the reattach early-return) so a pod that missed adoption on a prior
// bind is still adopted on re-drive.
func (w *RunStatusSandboxWriter) ObserveSandboxRef(ctx context.Context, runID, sandboxRef string) error {
	run, err := runByUIDFrom(ctx, w.client, runID)
	if err != nil {
		return fmt.Errorf("resolve Run %s for sandboxRef status: %w", runID, err)
	}
	if err := w.adoptSandboxPod(ctx, run, sandboxRef); err != nil {
		return err
	}
	if cur := run.Status.SandboxRef; cur != nil && cur.Name == sandboxRef {
		return nil // already observed (reattach path)
	}
	patch := []byte(fmt.Sprintf(
		`{"status":{"sandboxRef":{"name":%q,"namespace":%q}}}`,
		sandboxRef, run.Namespace))
	if err := w.client.Status().Patch(ctx, run, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("patch Run %s/%s status.sandboxRef: %w", run.Namespace, run.Name, err)
	}
	return nil
}

// adoptSandboxPod sets the Run CR as the controller owner of the sandbox pod so
// Kubernetes garbage-collects the pod when the Run is deleted (ISI-4366). It is
// idempotent: an already-adopted pod is left untouched, and a pod that already
// carries the ref returns without a write. A missing pod is not an error — the
// bind may race a teardown, and status.sandboxRef is still worth recording.
//
// blockOwnerDeletion is intentionally left unset: the goal is to reap the pod on
// Run deletion (background GC, which needs only the ownerReference), not to hold
// the Run open until the pod drains. Leaving it unset also avoids the
// ownerReferencesPermissionEnforcement admission check that would require
// update on the Run's finalizers subresource.
func (w *RunStatusSandboxWriter) adoptSandboxPod(ctx context.Context, run *api.Run, sandboxRef string) error {
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: run.Namespace, Name: sandboxRef}
	if err := w.client.Get(ctx, key, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // pod raced away; nothing to adopt
		}
		return fmt.Errorf("get sandbox pod %s/%s for adoption: %w", key.Namespace, key.Name, err)
	}
	for i := range pod.OwnerReferences {
		if pod.OwnerReferences[i].UID == run.UID {
			return nil // already adopted
		}
	}
	before := pod.DeepCopy()
	pod.OwnerReferences = append(pod.OwnerReferences, metav1.OwnerReference{
		APIVersion: api.GroupVersion.String(),
		Kind:       "Run",
		Name:       run.Name,
		UID:        run.UID,
		Controller: ptr.To(true),
	})
	if err := w.client.Patch(ctx, &pod, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("adopt sandbox pod %s/%s under Run %s: %w", key.Namespace, key.Name, run.Name, err)
	}
	return nil
}
