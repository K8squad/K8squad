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

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// StepSource reads the committed durable reconcile_step for a Run out of the
// coordination store. The production implementation wraps
// pkg/coord.ProdReconcileStore (keyed by the Run's work_item_id); this interface
// is the ONLY seam through which the controller learns Run state, which keeps the
// reconciler unit-testable against a fake and defers the Postgres wiring (and its
// real-DB integration gate, Story 2.7) to its own slice.
type StepSource interface {
	// StepForRun returns the committed reconcile_step for the Run identified by
	// its stable metadata.uid. found=false means no coord claim row exists yet
	// (the Run has not been admitted into the coordination DB); the reconciler
	// treats that as the initial Pending step rather than an error.
	StepForRun(ctx context.Context, runUID string) (step reconcile.Step, found bool, err error)
}

// Clock returns the timestamp stamped onto condition transitions. It is a field
// so tests pin it and a no-op requeue produces byte-identical status.
type Clock func() metav1.Time

// Reconciler projects the durable reconcile_step onto Run.status through the
// status subresource (arch §5.1/§8, AC2). It writes ONLY status, never spec, and
// is idempotent: a requeue whose durable step is unchanged recomputes an
// identical status and skips the patch entirely.
type Reconciler struct {
	client.Client
	Source StepSource
	// Now defaults to metav1.Now when nil.
	Now Clock
}

// Reconcile reads the Run, looks up its committed durable step, projects that
// onto status, and patches the status subresource only when it actually changed.
// A missing Run is not an error (it was deleted mid-queue). A StepSource error is
// returned so controller-runtime requeues with backoff rather than the loop
// treating a transient DB stall as a terminal state.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var runObj api.Run
	if err := r.Get(ctx, req.NamespacedName, &runObj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	step, found, err := r.Source.StepForRun(ctx, string(runObj.UID))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read durable step for run %s: %w", req.NamespacedName, err)
	}
	if !found {
		// No coord claim row yet: the Run is admitted but not enrolled in the
		// coordination DB. Pending is the truthful projection until it is.
		step = reconcile.StepPending
	}

	now := metav1.Now()
	if r.Now != nil {
		now = r.Now()
	}

	desired := ProjectStatus(runObj.Status, step, runObj.Generation, now)
	if apiequality.Semantic.DeepEqual(runObj.Status, desired) {
		return ctrl.Result{}, nil
	}

	patched := runObj.DeepCopy()
	patched.Status = desired
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(&runObj)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch run status %s: %w", req.NamespacedName, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler for Run objects. The manager-managed
// client is adopted when one was not injected (tests inject a fake).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.Run{}).
		Named("run").
		Complete(r)
}
