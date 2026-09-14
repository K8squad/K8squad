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
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/K8squad/K8squad/pkg/telemetry/cphealth"
)

// DefaultResync bounds how long a non-terminal Run's status projection can lag
// the durable coord step (ISI-4195). The projector watches ONLY the Run CR, but
// the source of truth is the coord reconcile_step in Postgres — a step the
// Driver advances WITHOUT touching the Run CR. Normally frequent status
// side-channel patches (sandboxRef, context snapshot) re-trigger this loop and
// keep the projection current; but when coord races claiming → terminal with no
// late Run CR event (dispatch retry window, e.g. sandbox pod boot), nothing
// re-enqueues the Run and the stale projection (e.g. Claiming) sticks. A bounded
// requeue while non-terminal makes the projector re-read coord and self-heal.
// Terminal phases are absorbing (immutable), so they never requeue — steady
// state cost is zero.
const DefaultResync = 30 * time.Second

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

// SettleSource reads the durable a2a follow-settlement marker (migration 0019,
// ISI-4348-S1) for a work item — the read side of coord.a2a_dispatch.settled_at,
// keyed by the SAME work_item_id the StepSource uses (ADR-0020 §2.4). The
// production implementation is pkg/coord.ProdSettleReader.SettledForWorkItem.
//
// It exists so the projector can tell a Succeeded/Failed durable step (the §6.4
// step machine reached terminal) from true agent-done: for an a2a-dispatched Run
// the follow that owns the sandbox pod may still be tearing down after the step
// commits, and phase should not read terminal until the follow settles (S3, F6).
//
// Nil disables the finalize-window hold (S3 is opt-in): the projector then flips
// straight to the terminal phase on the durable step alone (the pre-S3 behaviour),
// so a deployment wired without the settlement reader is strictly non-regressing.
type SettleSource interface {
	// SettledForWorkItem answers, for a Run's spec.workItemRef, both whether an a2a
	// follow was ever dispatched for this work item and whether any dispatch lap has
	// durably settled (§8 retry laps; settled iff any lap carries settled_at):
	//   dispatched=false → not an a2a follow (or never dispatched): no finalize
	//     window, project the terminal step unchanged.
	//   dispatched=true, settled=false → follow in flight: the finalize window is open.
	//   dispatched=true, settled=true → the follow completed durably.
	SettledForWorkItem(ctx context.Context, workItemID string) (dispatched, settled bool, err error)
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
	// Settlement holds the projected phase at Running through the a2a finalize
	// window (S3, ISI-4403): a Succeeded/Failed durable step whose follow has not
	// yet durably settled projects Running, not terminal, so phase reflects true
	// agent-done. Nil disables the hold — the projector flips on the step alone
	// (pre-S3 behaviour), which is why it is a separate opt-in seam, not folded
	// into StepSource.
	Settlement SettleSource
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
	// Resync is the non-terminal requeue cadence (ISI-4195); zero uses
	// DefaultResync. Tests pin it to observe the resync behaviour.
	Resync time.Duration
	// Health, when set, records this controller's reconcile latency + error
	// count onto the operator's OTel meter (ISI-4384/WS-E). Nil is a no-op.
	Health *cphealth.Metrics
	// PhaseKicks re-enqueues a Run for immediate re-projection when the driver
	// commits a durable step transition (ISI-4381 Option A). It is the
	// event-driven half of the projector's re-trigger: without it the projector
	// leans on the coarse non-terminal resync alone and short-lived intermediate
	// phases can elapse between two samples, so the CR appears to jump straight
	// to Succeeded. Nil disables the watch (unit tests, or a resync-only
	// deployment) — the resync stays the backstop for any dropped kick.
	PhaseKicks <-chan event.TypedGenericEvent[client.Object]
	// Recorder emits a Normal Kubernetes Event on each observed phase transition
	// so `kubectl describe run` / `kubectl get events` show the lifecycle
	// (ISI-4381 polish). Nil disables event emission (unit tests, or when no
	// recorder is wired); SetupWithManager defaults it to the manager's recorder.
	Recorder record.EventRecorder
}

// isFinalizableStep reports whether a durable step has an a2a follow finalize
// window the S3 hold applies to. Succeeded and Failed both settle their follow
// (coord.SettleOutcome{Succeeded,Failed,FollowError} all write settled_at), so
// both can be held at Running until settlement lands. Cancelled is out of scope:
// its teardown is the operator-driven Cancelling window, not an a2a follow settle.
func isFinalizableStep(s reconcile.Step) bool {
	return s == reconcile.StepSucceeded || s == reconcile.StepFailed
}

// resync returns the configured non-terminal requeue cadence, or the default.
func (r *Reconciler) resync() time.Duration {
	if r.Resync > 0 {
		return r.Resync
	}
	return DefaultResync
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

	// S3 finalize-window hold (ADR-0020 §2.4 / F6, ISI-4403): a Succeeded/Failed
	// durable step means the §6.4 step machine reached terminal, but for an
	// a2a-dispatched Run the follow that owns the sandbox pod may still be tearing
	// down. Until the durable settlement marker (migration 0019) lands, project the
	// pre-terminal StepCollecting so phase reads Running — `phase=Succeeded/Failed`
	// then reflects true agent-done, not step-done, and the Run's RBAC/MCP
	// side-channels stay converged until the agent is truly finished. Non-a2a Runs
	// (no dispatch row) and already-settled Runs project their terminal step
	// unchanged; a nil Settlement source disables the hold entirely (S3 opt-in).
	// The non-terminal projection requeues on the resync cadence below, which is
	// the backstop that flips Running → terminal once settlement lands (settlement
	// is not a step transition, so it does not ride the PhaseKicks channel).
	if r.Settlement != nil && isFinalizableStep(step) {
		dispatched, settled, serr := r.Settlement.SettledForWorkItem(ctx, runObj.Spec.WorkItemRef)
		if serr != nil {
			return ctrl.Result{}, fmt.Errorf("read a2a settlement for run %s: %w", req.NamespacedName, serr)
		}
		if dispatched && !settled {
			step = reconcile.StepCollecting
		}
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

	// A non-terminal projection requeues on a bounded cadence so a coord step
	// that advances without a Run CR event (ISI-4195 dispatch race) still gets
	// re-read and projected. Terminal phases are absorbing — no requeue. This
	// MUST ride the no-op path below too: the stuck case is precisely a stale
	// projection that DeepEquals itself, so returning bare nil there would kill
	// the only loop that could self-heal it.
	result := ctrl.Result{}
	if !isTerminalPhase(desired.Phase) {
		result.RequeueAfter = r.resync()
	}

	if apiequality.Semantic.DeepEqual(runObj.Status, desired) {
		return result, nil
	}

	prevPhase := runObj.Status.Phase
	patched := runObj.DeepCopy()
	patched.Status = desired
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(&runObj)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch run status %s: %w", req.NamespacedName, err)
	}
	// A committed phase change gets a Normal Event so the lifecycle shows up in
	// `kubectl describe run` (ISI-4381 polish). Only the phase field gates the
	// event: a condition/side-channel-only patch that leaves the phase put emits
	// nothing, so the event stream mirrors the phase transitions and no more.
	if r.Recorder != nil && desired.Phase != prevPhase {
		r.Recorder.Eventf(patched, corev1.EventTypeNormal, "Phase"+string(desired.Phase),
			"Run phase %s", desired.Phase)
	}
	return result, nil
}

// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager registers the reconciler for Run objects. The manager-managed
// client is adopted when one was not injected (tests inject a fake).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Recorder == nil {
		// record.EventRecorder (the "old" events API) is the ergonomic fit for a
		// single-line phase Event; controller-runtime's GetEventRecorder returns
		// the heavier events.EventRecorder (requires action + related object).
		//nolint:staticcheck // SA1019: old events API is intentional here.
		r.Recorder = mgr.GetEventRecorderFor("run")
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&api.Run{}).
		Named("run")
	if r.PhaseKicks != nil {
		// The driver's per-transition kick (ISI-4381 Option A): a step advance
		// re-enqueues the Run here so the new phase is projected at once.
		b = b.WatchesRawSource(source.Channel(r.PhaseKicks, &handler.EnqueueRequestForObject{}))
	}
	// Wrap the reconciler so its latency + error count land on the operator's
	// OTel meter (ISI-4384/WS-E); WrapReconciler is a no-op when Health is nil.
	return b.Complete(cphealth.WrapReconciler(r.Health, "run", r))
}
