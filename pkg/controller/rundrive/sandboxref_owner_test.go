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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// ISI-4366: warm-pool sandbox pods are booted with NO ownerReference. If a Run
// CR is deleted while non-terminal (the terminal release path never runs), the
// pod is stranded with no owner for Kubernetes GC to reap. ObserveSandboxRef
// therefore adopts the pod under the Run at bind time, so pod GC is guaranteed
// even when the Release/terminal-projection path is skipped.

const (
	ownerRunUID = "44444444-4444-4444-4444-444444444444"
	ownerPod    = "sandbox-9f02f4f3aa11bc22"
)

// ownedPodFixture builds a fake client holding a Run (keyed by uid) and its
// bound sandbox pod (no owner, mirroring a warm-pool boot), plus the writer.
func ownedPodFixture(t *testing.T, mutatePod func(*corev1.Pod)) (*RunStatusSandboxWriter, client.Client) {
	t.Helper()
	run := newTestRun(ownerRunUID, "wi-owner")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: ownerPod, Namespace: run.Namespace},
	}
	objs := []client.Object{run}
	if mutatePod != nil {
		mutatePod(pod)
	}
	if pod != nil {
		objs = append(objs, pod)
	}
	cl := fake.NewClientBuilder().
		WithScheme(goneScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&api.Run{}).
		Build()
	return NewRunStatusSandboxWriter(cl), cl
}

func getPod(t *testing.T, cl client.Client, name string) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, cl.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: name}, &pod))
	return &pod
}

// TestObserveSandboxRefAdoptsPodUnderRun: an unowned sandbox pod gets a
// controller ownerReference to its Run at bind, and the status ref is written.
func TestObserveSandboxRefAdoptsPodUnderRun(t *testing.T) {
	w, cl := ownedPodFixture(t, nil)

	require.NoError(t, w.ObserveSandboxRef(context.Background(), ownerRunUID, ownerPod))

	pod := getPod(t, cl, ownerPod)
	require.Len(t, pod.OwnerReferences, 1, "the pod is adopted under exactly one owner")
	or := pod.OwnerReferences[0]
	assert.Equal(t, "Run", or.Kind)
	assert.Equal(t, api.GroupVersion.String(), or.APIVersion)
	assert.Equal(t, "run-1", or.Name)
	assert.Equal(t, types.UID(ownerRunUID), or.UID,
		"the ownerReference UID is the Run uid GC keys on")
	require.NotNil(t, or.Controller)
	assert.True(t, *or.Controller, "the Run is the controller owner")

	// status.sandboxRef is still recorded (the pre-existing behavior).
	var run api.Run
	require.NoError(t, cl.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "run-1"}, &run))
	require.NotNil(t, run.Status.SandboxRef)
	assert.Equal(t, ownerPod, run.Status.SandboxRef.Name)
}

// TestDeletedNonTerminalRunLeavesNoOrphanPod is the ISI-4366 regression: after
// the bind-time adoption, a non-terminal Run deleted WITHOUT the release path
// ever running leaves the pod carrying an ownerReference whose owner no longer
// exists — exactly the state Kubernetes' garbage collector reaps. The fake
// client does not run the GC controller, so we assert the load-bearing
// contract directly: the pod is owned (Controller=true) by the deleted Run's
// uid. Before this fix the pod had zero ownerReferences and would be orphaned.
func TestDeletedNonTerminalRunLeavesNoOrphanPod(t *testing.T) {
	w, cl := ownedPodFixture(t, nil)
	ctx := context.Background()

	require.NoError(t, w.ObserveSandboxRef(ctx, ownerRunUID, ownerPod))

	// Force-delete the Run while it is still non-terminal (no Release call).
	var run api.Run
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "run-1"}, &run))
	require.NoError(t, cl.Delete(ctx, &run))

	pod := getPod(t, cl, ownerPod)
	require.Len(t, pod.OwnerReferences, 1,
		"the pod carries an owner GC can follow — not an orphan")
	assert.Equal(t, types.UID(ownerRunUID), pod.OwnerReferences[0].UID)
	require.NotNil(t, pod.OwnerReferences[0].Controller)
	assert.True(t, *pod.OwnerReferences[0].Controller)

	// The owner is gone: GC would now reap this pod (nothing else owns it).
	err := cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "run-1"}, &api.Run{})
	assert.Error(t, err, "the Run is deleted; its owned pod is GC-eligible")
}

// TestObserveSandboxRefAdoptionIdempotent: re-observing an already-adopted pod
// (reattach / re-drive) does not append a duplicate ownerReference.
func TestObserveSandboxRefAdoptionIdempotent(t *testing.T) {
	w, cl := ownedPodFixture(t, nil)
	ctx := context.Background()

	require.NoError(t, w.ObserveSandboxRef(ctx, ownerRunUID, ownerPod))
	require.NoError(t, w.ObserveSandboxRef(ctx, ownerRunUID, ownerPod))

	pod := getPod(t, cl, ownerPod)
	assert.Len(t, pod.OwnerReferences, 1, "adoption is idempotent across re-drives")
}

// TestObserveSandboxRefAdoptsOnReattach: adoption runs even when status is
// already set (the reattach early-return path), so a pod that missed adoption
// on a prior bind is still adopted.
func TestObserveSandboxRefAdoptsOnReattach(t *testing.T) {
	run := newTestRun(ownerRunUID, "wi-owner")
	run.Status.SandboxRef = &api.ObjectRef{Name: ownerPod, Namespace: run.Namespace}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: ownerPod, Namespace: run.Namespace}}
	cl := fake.NewClientBuilder().
		WithScheme(goneScheme(t)).
		WithObjects(run, pod).
		WithStatusSubresource(&api.Run{}).
		Build()
	w := NewRunStatusSandboxWriter(cl)

	require.NoError(t, w.ObserveSandboxRef(context.Background(), ownerRunUID, ownerPod))

	got := getPod(t, cl, ownerPod)
	require.Len(t, got.OwnerReferences, 1,
		"reattach still adopts a pod that missed adoption on a prior bind")
	assert.Equal(t, types.UID(ownerRunUID), got.OwnerReferences[0].UID)
}

// TestObserveSandboxRefMissingPodStillWritesStatus: a pod that raced away
// before adoption is not an error — the status ref is still recorded.
func TestObserveSandboxRefMissingPodStillWritesStatus(t *testing.T) {
	// No pod object in the client.
	run := newTestRun(ownerRunUID, "wi-owner")
	cl := fake.NewClientBuilder().
		WithScheme(goneScheme(t)).
		WithObjects(run).
		WithStatusSubresource(&api.Run{}).
		Build()
	w := NewRunStatusSandboxWriter(cl)

	require.NoError(t, w.ObserveSandboxRef(context.Background(), ownerRunUID, ownerPod),
		"a missing pod is tolerated: the bind may race a teardown")

	var got api.Run
	require.NoError(t, cl.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "run-1"}, &got))
	require.NotNil(t, got.Status.SandboxRef)
	assert.Equal(t, ownerPod, got.Status.SandboxRef.Name)
}
