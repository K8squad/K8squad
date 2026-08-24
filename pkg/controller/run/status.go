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

// Package run hosts the Run controller: the CRD-status projection of the durable
// reconcile machine (pkg/reconcile, ISI-2535) onto Run.status, and the
// controller-runtime reconciler that patches it (ISI-2655, Story 3.1).
//
// The load-bearing rule (arch §5.1/§8, AC2): status is DOWNSTREAM of the durable
// reconcile_step. The coord Postgres reconcile_step + fence is the Run's source
// of truth (pkg/coord.ProdReconcileStore); Run.status is a read-only PROJECTION
// of it. ProjectStatus is the pure, deterministic mapping — the reconciler reads
// the committed step from the Store and applies this projection under the status
// subresource, never inventing state the durable step does not already justify.
// This retires the naive in-memory Run simulation: the phase you observe is the
// phase the durable step commands, no more.
package run

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// ConditionReady is the summary condition: True only when the Run has reached the
// terminal Succeeded step. Its Reason carries the granular §5.2 sub-state
// (e.g. Paused(rate_limited)) so operators can distinguish holds from failures
// without decoding the coarse Phase enum.
const ConditionReady = "Ready"

// Ready condition reasons. Each is a valid k8s condition reason
// (^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$) and maps 1:1 to a durable step class.
const (
	reasonReconciling = "Reconciling"
	reasonPaused      = "Paused"
	reasonRateLimited = "RateLimited"
	reasonSucceeded   = "Succeeded"
	reasonFailed      = "Failed"
	reasonCancelled   = "Cancelled"
	reasonUnknownStep = "UnknownStep"
)

// PhaseOf bridges the reconcile machine's coarse Phase (pkg/reconcile) onto the
// CRD's RunPhase enum. The two enums are value-identical by construction (arch
// §5.1 r28 / §8); this switch keeps the bridge explicit and fail-closed — an
// unrecognised step projects to the empty phase rather than a guessed one, so a
// schema drift surfaces as an admission failure instead of a silent mislabel.
func PhaseOf(step reconcile.Step) api.RunPhase {
	switch reconcile.PhaseOf(step) {
	case reconcile.PhasePending:
		return api.RunPhasePending
	case reconcile.PhaseClaiming:
		return api.RunPhaseClaiming
	case reconcile.PhaseRunning:
		return api.RunPhaseRunning
	case reconcile.PhasePaused:
		return api.RunPhasePaused
	case reconcile.PhaseSucceeded:
		return api.RunPhaseSucceeded
	case reconcile.PhaseFailed:
		return api.RunPhaseFailed
	case reconcile.PhaseCancelled:
		return api.RunPhaseCancelled
	default:
		return ""
	}
}

// ProjectStatus computes the Run.status that the durable step commands, merged
// onto the current status so operator-invisible fields survive (SandboxRef,
// ArtifactRefs, ClaimedAt, ModelSegments — those are written by the
// effects/claim/fallback-switch paths, not by this projection). It is pure:
// same (current, step, generation, now) always
// yields the same status, which is what makes the reconciler idempotent and the
// projection unit-testable without a cluster.
//
// generation is the Run's metadata.generation the reconciler observed; stamping
// it onto both status.observedGeneration and the Ready condition lets clients
// tell a fresh observation from a stale one (§5.2). now is injected (not read
// from the clock) so the LastTransitionTime is deterministic in tests and stable
// across a no-op requeue.
func ProjectStatus(current api.RunStatus, step reconcile.Step, generation int64, now metav1.Time) api.RunStatus {
	out := *current.DeepCopy()
	out.Phase = PhaseOf(step)
	out.ObservedGeneration = generation
	meta.SetStatusCondition(&out.Conditions, readyCondition(step, generation, now))
	return out
}

// readyCondition derives the summary Ready condition from the durable step.
// Ready is True ONLY at Succeeded; every other step (including the terminal
// Failed/Cancelled) is not-Ready with a reason that names the sub-state. The two
// paused steps share PhasePaused but keep distinct reasons so the rate-limit hold
// is legible.
func readyCondition(step reconcile.Step, generation int64, now metav1.Time) metav1.Condition {
	c := metav1.Condition{
		Type:               ConditionReady,
		ObservedGeneration: generation,
		LastTransitionTime: now,
	}
	switch step {
	case reconcile.StepSucceeded:
		c.Status = metav1.ConditionTrue
		c.Reason = reasonSucceeded
		c.Message = "Run completed successfully"
	case reconcile.StepFailed:
		c.Status = metav1.ConditionFalse
		c.Reason = reasonFailed
		c.Message = "Run reached a terminal failure"
	case reconcile.StepCancelled:
		c.Status = metav1.ConditionFalse
		c.Reason = reasonCancelled
		c.Message = "Run was cancelled by an operator"
	case reconcile.StepPaused:
		c.Status = metav1.ConditionFalse
		c.Reason = reasonPaused
		c.Message = "Run is paused"
	case reconcile.StepPausedRateLimited:
		c.Status = metav1.ConditionFalse
		c.Reason = reasonRateLimited
		c.Message = "Run is paused waiting on a rate-limit window"
	case reconcile.StepPending, reconcile.StepClaimingSandbox,
		reconcile.StepDispatching, reconcile.StepRunning, reconcile.StepCollecting:
		c.Status = metav1.ConditionFalse
		c.Reason = reasonReconciling
		c.Message = "Run is progressing toward completion"
	default:
		c.Status = metav1.ConditionUnknown
		c.Reason = reasonUnknownStep
		c.Message = "Run reconcile_step is not recognised by this controller"
	}
	return c
}
