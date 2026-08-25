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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// TestCancelFinishDrivesTeardownThenTerminal (3.3, ISI-2884): a Run at
// cancelling gets its sandbox torn down and the guarded finish → cancelled.
// The drive does NOT run the happy-path machine (kill is a caller-owned
// transition, never machine-driven).
func TestCancelFinishDrivesTeardownThenTerminal(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "wi-1")
	run.Status.Phase = api.RunPhaseCanceling
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()

	claims := &fakeClaims{
		found:  true,
		state:  ClaimState{Step: reconcile.StepCancelling, Fence: 7},
		cancelFinishOK: true,
	}
	releaser := &fakeReleaser{}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: &fakeMachineStore{}})
	d.Sandbox = releaser

	_, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	require.NoError(t, err)

	assert.Equal(t, []string{"11111111-1111-1111-1111-111111111111"}, releaser.released,
		"the killed Run's sandbox is torn down before the finish")
	assert.True(t, claims.cancelFinishCall, "the guarded cancelling → cancelled finish fires")
}

// TestCancelFinishFenceConflictRequeues: the fence moved under the finish
// (another kill/teardown raced) — re-read the world next pass, no error.
func TestCancelFinishFenceConflictRequeues(t *testing.T) {
	run := newTestRun("22222222-2222-2222-2222-222222222222", "wi-2")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()

	claims := &fakeClaims{
		found:  true,
		state:  ClaimState{Step: reconcile.StepCancelling, Fence: 9},
		cancelFinishOK: false, // fence lost
	}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: &fakeMachineStore{}})

	requeue, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	require.NoError(t, err)
	assert.Equal(t, continueDelay, requeue, "a lost fence requeues, never errors")
}

// TestCancelFinishWithoutSandbox: no physical sandbox (ledger-only pool) still
// finishes — teardown is best-effort sugar, the durable finish is the contract.
func TestCancelFinishWithoutSandbox(t *testing.T) {
	run := newTestRun("33333333-3333-3333-3333-333333333333", "wi-3")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()

	claims := &fakeClaims{
		found:  true,
		state:  ClaimState{Step: reconcile.StepCancelling, Fence: 3},
		cancelFinishOK: true,
	}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: &fakeMachineStore{}})
	d.Sandbox = nil

	_, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	require.NoError(t, err)
	assert.True(t, claims.cancelFinishCall)
}

// TestOnCancelDueKicksWorkItems: the kill sweep's due list kicks every owning
// Run back into the drive loop through the resume channel.
func TestOnCancelDueKicksWorkItems(t *testing.T) {
	run := newTestRun("44444444-4444-4444-4444-444444444444", "wi-4")
	run2 := newTestRun("55555555-5555-5555-5555-555555555555", "wi-4")
	run2.Name = "run-2"
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run, run2).
		WithIndex(&api.Run{}, workItemField,
			func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef } }).
		Build()

	d := newDriver(cl, &fakeClaims{}, &fakePauses{}, &fakeRunner{store: &fakeMachineStore{}})

	d.OnCancelDue(context.Background(), []string{"wi-4"})

	kicked := 0
	for {
		select {
		case <-d.ResumeEvents():
			kicked++
		default:
			assert.GreaterOrEqual(t, kicked, 1, "the due work item's Run(s) get kicked")
			return
		}
	}
}

// TestCancelSweeperTicksAndDelegates: the sweeper lists CancelDue on its tick
// and hands the result to OnDue; a Claims error is logged, not fatal.
func TestCancelSweeperTicksAndDelegates(t *testing.T) {
	claims := &fakeClaims{cancelDue: []string{"wi-a"}, cancelDueErr: nil}
	var got [][]string
	s := &CancelSweeper{
		Claims: claims,
		OnDue:  func(_ context.Context, due []string) { got = append(got, due) },
		Tick:   time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.NoError(t, s.Start(ctx))
	// The static fake re-lists the same due item every tick, so the sweep
	// delegates it at least once over the window; the contract under test is
	// tick → CancelDue → OnDue, not the exact tick count.
	require.GreaterOrEqual(t, len(got), 1)
	assert.Equal(t, []string{"wi-a"}, got[0])
}

// TestCancellingNotDeadNotDriven: a cancelling claim is not death-detected
// (the holder/lease were cleared at CancelEnter) and never reaches the
// happy-path machine drive.
func TestCancellingNotDeadNotDriven(t *testing.T) {
	run := newTestRun("66666666-6666-6666-6666-666666666666", "wi-6")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()

	store := &fakeMachineStore{step: reconcile.StepRunning}
	claims := &fakeClaims{
		found:           true,
		state:           ClaimState{Step: reconcile.StepCancelling, Fence: 1},
		cancelFinishOK:  true,
	}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store})

	_, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	require.NoError(t, err)
	assert.Equal(t, 0, store.advances, "the machine never drives a cancelling Run")
}
