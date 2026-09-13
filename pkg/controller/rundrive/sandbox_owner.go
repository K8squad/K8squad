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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch

// adoptSandboxPodToRun stamps a controller ownerReference tying the just-bound
// sandbox pod to its owning Run (ISI-4366). Warm-pool pods are created BEFORE
// any Run exists (warmpool.Boot), so they carry no owner; without one, a Run CR
// force-deleted while still non-terminal never runs its terminal release path
// and the pod orphans with no owner for Kubernetes GC to reap (observed at 500m
// each on k8squad-test 2026-09-12). Stamping the owner at bind makes pod GC
// guaranteed the moment the Run is deleted, independent of the terminal
// projection/release path.
//
// It is idempotent (a re-drive/reattach re-observes the same ref harmlessly)
// and fail-safe on the legacy default-namespace path: a namespaced owner and
// its dependent must share a namespace, so when the pod is not in the Run's
// namespace it is left alone — a cross-namespace ownerReference would make GC
// treat the owner as missing and delete the pod immediately. Classified pods
// (ADR-044 step-9 key) always share the Run's namespace, the case this closes.
func adoptSandboxPodToRun(ctx context.Context, c client.Client, run *api.Run, sandboxRef string) error {
	pod, err := findSandboxPod(ctx, c, sandboxRef)
	if err != nil {
		return err
	}
	// Cross-namespace ownerReferences are invalid for a namespaced owner; skip
	// rather than strand the pod under an owner GC considers missing.
	if pod.Namespace != run.Namespace {
		return nil
	}
	// Idempotent: a controller owner already present (re-bind/reattach) is a
	// no-op — never fight another controller for ownership.
	if metav1.GetControllerOf(pod) != nil {
		return nil
	}
	ref := metav1.OwnerReference{
		APIVersion: api.GroupVersion.String(),
		Kind:       "Run",
		Name:       run.Name,
		UID:        run.UID,
		Controller: ptrTo(true),
		// GC on Run deletion is the whole point; blocking/foreground-ordering
		// the Run's own deletion on the pod is not — and BlockOwnerDeletion:true
		// would also demand runs/finalizers update RBAC we do not need here.
		BlockOwnerDeletion: ptrTo(false),
	}
	patched := pod.DeepCopy()
	patched.OwnerReferences = append(patched.OwnerReferences, ref)
	if err := c.Patch(ctx, patched, client.MergeFrom(pod)); err != nil {
		return fmt.Errorf("stamp Run ownerReference on sandbox pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// ptrTo returns a pointer to v — a local helper for the optional pointer fields
// on OwnerReference.
func ptrTo[T any](v T) *T { return &v }
