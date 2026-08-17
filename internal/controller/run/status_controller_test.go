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

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// validRunPhases is the CRD's enum-validated phase set (api §8). A projected
// phase outside this set would fail admission (fail-closed) — so the projection
// mapping MUST land inside it for every reachable durable step.
var validRunPhases = map[ksquadv1alpha1.RunPhase]bool{
	ksquadv1alpha1.RunPhasePending:   true,
	ksquadv1alpha1.RunPhaseClaiming:  true,
	ksquadv1alpha1.RunPhaseRunning:   true,
	ksquadv1alpha1.RunPhasePaused:    true,
	ksquadv1alpha1.RunPhaseSucceeded: true,
	ksquadv1alpha1.RunPhaseFailed:    true,
	ksquadv1alpha1.RunPhaseCancelled: true,
}

// allSteps is every durable reconcile_step the machine can persist (happy path +
// terminal + paused tiers). The projection must map each to a valid CRD phase.
var allSteps = []reconcile.Step{
	reconcile.StepPending,
	reconcile.StepClaimingSandbox,
	reconcile.StepDispatching,
	reconcile.StepRunning,
	reconcile.StepCollecting,
	reconcile.StepSucceeded,
	reconcile.StepFailed,
	reconcile.StepCancelled,
	reconcile.StepPaused,
	reconcile.StepPausedRateLimited,
}

// TestProjectionIsTotalAndValid asserts the durable-step → RunPhase projection
// (the cast in StatusReconciler.project) is total over every persistable step and
// only ever yields a CRD-admissible phase. This is the fail-closed guard: a new
// step whose PhaseOf mapping drifts outside the enum would break the CRD status
// write, and this test catches it at build time.
func TestProjectionIsTotalAndValid(t *testing.T) {
	for _, s := range allSteps {
		phase := ksquadv1alpha1.RunPhase(string(reconcile.PhaseOf(s)))
		if phase == "" {
			t.Fatalf("step %q projected to an empty phase (would clear Run.status.phase)", s)
		}
		if !validRunPhases[phase] {
			t.Fatalf("step %q projected to %q, which is not a CRD-admissible RunPhase", s, phase)
		}
	}
}

// TestTerminalStepsProjectTerminalPhases guards the absorbing mapping: a Run whose
// durable step is terminal must not project as an in-flight phase (which would
// make a finished Run look live in the console).
func TestTerminalStepsProjectTerminalPhases(t *testing.T) {
	terminal := map[reconcile.Step]ksquadv1alpha1.RunPhase{
		reconcile.StepSucceeded: ksquadv1alpha1.RunPhaseSucceeded,
		reconcile.StepFailed:    ksquadv1alpha1.RunPhaseFailed,
		reconcile.StepCancelled: ksquadv1alpha1.RunPhaseCancelled,
	}
	for step, want := range terminal {
		if !reconcile.IsTerminal(step) {
			t.Fatalf("expected %q to be a terminal step", step)
		}
		got := ksquadv1alpha1.RunPhase(string(reconcile.PhaseOf(step)))
		if got != want {
			t.Fatalf("terminal step %q projected to %q, want %q", step, got, want)
		}
	}
}
