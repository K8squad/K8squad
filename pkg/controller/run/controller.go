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
// coordination store. The production implementation is
// pkg/coord.ReconcileStepReader (the read-only side of the §6.4 durable step,
// keyed by work_item_id); this interface is the ONLY seam through which the
// controller learns Run state, which keeps the reconciler unit-testable against a
// fake and confines the Postgres dependency to the operator wiring + its real-DB
// integration gate (Story 2.7).
type StepSource interface {
	// StepForWorkItem returns the committed reconcile_step for the coord.claim row
	// keyed by workItemID — the Run's spec.workItemRef, the opaque coordination-DB
	// pointer (ADR-001), NOT the Run's k8s uid. found=false means no claim row
	// exists yet (the Run is admitted but not enrolled in coord); the reconciler
	// treats that as the initial Pending step rather than an error.
	StepForWorkItem(ctx context.Context, workItemID string) (step reconcile.Step, found bool, err error)
}

// Clock returns the timestamp stamped onto condition transitions. It is a field
// so tests pin it and a no-op requeue produces byte-identical status.
type Clock func() metav1.Time

// Reconciler projects the durable reconcile_step onto Run.status through the
// status subresource (arch §5.1/§8, AC2). It writes ONLY status, never spec, and
// is idempotent: a requeue whose durable step is unchanged recomputes an
// identical status and skips the patch entirely.
//
// Epic B (ISI-3286) adds the toolchain RBAC side-channel: while the Run is
// live, the RBAC renderer keeps the per-Run Role union (bound to the managed
// ksquad-agent SA) converged to the Run's resolved toolchains and records the
// union on status; when the step goes terminal, the rendered objects are
// released and the record cleared (acceptance 3b).
//
// Epic C (ISI-3287) adds the capability-manifest side-channel: pre-dispatch,
// the assembler resolves the Run's full capability envelope fail-closed,
// stamps status.capabilityManifest (immutable for the Run's life — the
// audit/reproducibility truth, kept at terminal) and projects the MCP IR
// ConfigMap the runtime adapters consume.
type Reconciler struct {
	client.Client
	Source StepSource
	// Now defaults to metav1.Now when nil.
	Now Clock
	// RBAC renders the per-Run toolchain Role union. Nil disables the
	// side-channel (unit tests of the pure status projection).
	RBAC *RBACRenderer
	// Assembler resolves and records the capability manifest (Epic C).
	// Nil disables the side-channel.
	Assembler *Assembler
	// ContextAssemblers builds the §8.5 context assembler over the
	// production sources (story S1, ISI-3600). Nil disables the context
	// side-channel — dispatch then ships title+body only (the pre-S1
	// behavior), so the field is opt-in and non-regressing.
	ContextAssemblers ContextAssemblers
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

	step, found, err := r.Source.StepForWorkItem(ctx, runObj.Spec.WorkItemRef)
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

	// Toolchain RBAC side-channel (Epic B): converge while live, release on
	// terminal. Fail-closed — a resolution or render error requeues rather
	// than letting a Run proceed with partial (or stale) grants.
	if r.RBAC != nil {
		if isTerminalPhase(desired.Phase) {
			if err := r.RBAC.Release(ctx, &runObj); err != nil {
				return ctrl.Result{}, fmt.Errorf("release toolchain rbac for run %s: %w", req.NamespacedName, err)
			}
			desired.GrantedToolchainRBAC = nil
		} else {
			grant, err := r.RBAC.Ensure(ctx, &runObj)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("render toolchain rbac for run %s: %w", req.NamespacedName, err)
			}
			desired.GrantedToolchainRBAC = grant
		}
	}

	// Capability-manifest side-channel (Epic C): compute pre-dispatch,
	// immutable afterwards — the recorded manifest IS the audit truth, so
	// unlike the RBAC grant it survives the Run going terminal. Fail-closed:
	// an unresolvable envelope requeues (a Run never dispatches with a
	// partial capability plane).
	if r.Assembler != nil {
		if isTerminalPhase(desired.Phase) {
			// The IR projection follows the Run's life; the manifest record
			// stays (owner-ref GC removes the ConfigMap with the object;
			// this sweep covers the terminal-but-not-deleted drift case).
			if err := r.Assembler.ReleaseConfig(ctx, &runObj); err != nil {
				return ctrl.Result{}, fmt.Errorf("release mcp config for run %s: %w", req.NamespacedName, err)
			}
			desired.CapabilityManifest = runObj.Status.CapabilityManifest
		} else {
			manifest, err := r.Assembler.EnsureManifest(ctx, &runObj)
			if err != nil {
				return ctrl.Result{}, wrapAssemblyError(&runObj, err)
			}
			desired.CapabilityManifest = manifest
		}
	}

	// Context-assembler side-channel (story S1, ISI-3600): at the Claiming →
	// Running transition, assemble the §8.5 context envelope and pin its
	// resolved-input snapshot on status.contextSnapshot (immutable for the
	// Run's life, like the capability manifest). Dispatch re-reads the pinned
	// snapshot to inject the identical context (deterministic resume).
	// Fail-closed: an assembly error requeues — a Run never dispatches with a
	// partial context envelope.
	if err := r.ensureContextSnapshot(ctx, &runObj, &desired); err != nil {
		return ctrl.Result{}, err
	}

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
