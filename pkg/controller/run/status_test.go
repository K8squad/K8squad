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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

var fixedNow = metav1.NewTime(time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC))

func TestPhaseOf(t *testing.T) {
	cases := []struct {
		step reconcile.Step
		want api.RunPhase
	}{
		{reconcile.StepPending, api.RunPhasePending},
		{reconcile.StepClaimingSandbox, api.RunPhaseClaiming},
		{reconcile.StepDispatching, api.RunPhaseClaiming},
		{reconcile.StepRunning, api.RunPhaseRunning},
		{reconcile.StepCollecting, api.RunPhaseRunning},
		{reconcile.StepPaused, api.RunPhasePaused},
		{reconcile.StepPausedRateLimited, api.RunPhasePaused},
		{reconcile.StepSucceeded, api.RunPhaseSucceeded},
		{reconcile.StepFailed, api.RunPhaseFailed},
		{reconcile.StepCancelled, api.RunPhaseCancelled},
		{reconcile.Step("gremlin"), api.RunPhase("")}, // fail-closed on drift
	}
	for _, tc := range cases {
		if got := PhaseOf(tc.step); got != tc.want {
			t.Errorf("PhaseOf(%q) = %q, want %q", tc.step, got, tc.want)
		}
	}
}

// TestPhaseOfCoversEveryStep guards against a new reconcile step being added
// without teaching the bridge about it (the default arm would silently return "").
func TestPhaseOfCoversEveryStep(t *testing.T) {
	allSteps := []reconcile.Step{
		reconcile.StepPending, reconcile.StepClaimingSandbox, reconcile.StepDispatching,
		reconcile.StepRunning, reconcile.StepCollecting, reconcile.StepSucceeded,
		reconcile.StepFailed, reconcile.StepCancelled, reconcile.StepPaused,
		reconcile.StepPausedRateLimited,
	}
	for _, s := range allSteps {
		if PhaseOf(s) == "" {
			t.Errorf("PhaseOf(%q) returned empty phase for a known step", s)
		}
	}
}

func TestProjectStatusPhaseAndGeneration(t *testing.T) {
	got := ProjectStatus(api.RunStatus{}, reconcile.StepRunning, 7, fixedNow)
	if got.Phase != api.RunPhaseRunning {
		t.Errorf("Phase = %q, want Running", got.Phase)
	}
	if got.ObservedGeneration != 7 {
		t.Errorf("ObservedGeneration = %d, want 7", got.ObservedGeneration)
	}
	cond := meta.FindStatusCondition(got.Conditions, ConditionReady)
	if cond == nil {
		t.Fatalf("Ready condition missing")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != reasonReconciling {
		t.Errorf("Ready = %v/%s, want False/Reconciling", cond.Status, cond.Reason)
	}
	if cond.ObservedGeneration != 7 {
		t.Errorf("condition ObservedGeneration = %d, want 7", cond.ObservedGeneration)
	}
}

func TestProjectStatusReadyConditionPerStep(t *testing.T) {
	cases := []struct {
		step       reconcile.Step
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{reconcile.StepPending, metav1.ConditionFalse, reasonReconciling},
		{reconcile.StepClaimingSandbox, metav1.ConditionFalse, reasonReconciling},
		{reconcile.StepDispatching, metav1.ConditionFalse, reasonReconciling},
		{reconcile.StepRunning, metav1.ConditionFalse, reasonReconciling},
		{reconcile.StepCollecting, metav1.ConditionFalse, reasonReconciling},
		{reconcile.StepPaused, metav1.ConditionFalse, reasonPaused},
		{reconcile.StepPausedRateLimited, metav1.ConditionFalse, reasonRateLimited},
		{reconcile.StepSucceeded, metav1.ConditionTrue, reasonSucceeded},
		{reconcile.StepFailed, metav1.ConditionFalse, reasonFailed},
		{reconcile.StepCancelled, metav1.ConditionFalse, reasonCancelled},
		{reconcile.Step("gremlin"), metav1.ConditionUnknown, reasonUnknownStep},
	}
	for _, tc := range cases {
		got := ProjectStatus(api.RunStatus{}, tc.step, 1, fixedNow)
		cond := meta.FindStatusCondition(got.Conditions, ConditionReady)
		if cond == nil {
			t.Fatalf("step %q: Ready condition missing", tc.step)
		}
		if cond.Status != tc.wantStatus || cond.Reason != tc.wantReason {
			t.Errorf("step %q: Ready = %v/%s, want %v/%s",
				tc.step, cond.Status, cond.Reason, tc.wantStatus, tc.wantReason)
		}
	}
}

// TestProjectStatusPreservesEffectFields proves the projection is a merge, not a
// clobber: fields written by the claim/effects path (SandboxRef, ArtifactRefs,
// ClaimedAt) survive a status projection that only owns Phase/Conditions/gen.
func TestProjectStatusPreservesEffectFields(t *testing.T) {
	claimedAt := metav1.NewTime(time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC))
	current := api.RunStatus{
		SandboxRef:   &api.ObjectRef{Name: "sandbox-7", Namespace: "pool"},
		ClaimedAt:    &claimedAt,
		ArtifactRefs: []api.ObjectRef{{Name: "artifact-abc"}},
	}
	got := ProjectStatus(current, reconcile.StepRunning, 3, fixedNow)
	if got.SandboxRef == nil || got.SandboxRef.Name != "sandbox-7" {
		t.Errorf("SandboxRef not preserved: %+v", got.SandboxRef)
	}
	if got.ClaimedAt == nil || !got.ClaimedAt.Equal(&claimedAt) {
		t.Errorf("ClaimedAt not preserved: %+v", got.ClaimedAt)
	}
	if len(got.ArtifactRefs) != 1 || got.ArtifactRefs[0].Name != "artifact-abc" {
		t.Errorf("ArtifactRefs not preserved: %+v", got.ArtifactRefs)
	}
}

// TestProjectStatusIsPure re-runs the projection on its own output with the same
// inputs and expects an identical status — the property the reconciler relies on
// to skip a no-op patch.
func TestProjectStatusIsPure(t *testing.T) {
	first := ProjectStatus(api.RunStatus{}, reconcile.StepDispatching, 5, fixedNow)
	second := ProjectStatus(first, reconcile.StepDispatching, 5, fixedNow)
	if !equalStatus(first, second) {
		t.Errorf("projection not idempotent:\nfirst=%+v\nsecond=%+v", first, second)
	}
}

// TestProjectStatusTransitionUpdatesCondition proves LastTransitionTime advances
// only when the Ready status actually flips (Running→Succeeded), not on every
// pass.
func TestProjectStatusTransitionUpdatesCondition(t *testing.T) {
	running := ProjectStatus(api.RunStatus{}, reconcile.StepRunning, 1, fixedNow)
	later := metav1.NewTime(fixedNow.Add(time.Hour))

	// Same step at a later time: condition status unchanged, so the transition
	// time must be preserved (not bumped).
	stillRunning := ProjectStatus(running, reconcile.StepRunning, 1, later)
	rc := meta.FindStatusCondition(stillRunning.Conditions, ConditionReady)
	if !rc.LastTransitionTime.Equal(&fixedNow) {
		t.Errorf("LastTransitionTime bumped on no-op: got %v want %v", rc.LastTransitionTime, fixedNow)
	}

	// Flip to Succeeded: status changes True, so the transition time advances.
	done := ProjectStatus(running, reconcile.StepSucceeded, 1, later)
	dc := meta.FindStatusCondition(done.Conditions, ConditionReady)
	if dc.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True on Succeeded, got %v", dc.Status)
	}
	if !dc.LastTransitionTime.Equal(&later) {
		t.Errorf("LastTransitionTime not advanced on flip: got %v want %v", dc.LastTransitionTime, later)
	}
}

func equalStatus(a, b api.RunStatus) bool {
	if a.Phase != b.Phase || a.ObservedGeneration != b.ObservedGeneration {
		return false
	}
	if len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		x, y := a.Conditions[i], b.Conditions[i]
		if x.Type != y.Type || x.Status != y.Status || x.Reason != y.Reason ||
			x.ObservedGeneration != y.ObservedGeneration || !x.LastTransitionTime.Equal(&y.LastTransitionTime) {
			return false
		}
	}
	return true
}
