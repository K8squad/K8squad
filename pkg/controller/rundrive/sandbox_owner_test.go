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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

func nonTerminalRun(name, ns, uid string) *api.Run {
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid)},
		Spec:       api.RunSpec{WorkItemRef: "10000000-0000-0000-0000-000000000001"},
		Status:     api.RunStatus{Phase: api.RunPhaseRunning},
	}
}

// ISI-4366 regression: when the warm pool binds a sandbox to a Run, the bind
// observer stamps the Run as the pod's controller ownerReference. That owner
// reference is exactly the contract Kubernetes GC keys on — a non-terminal Run
// force-deleted (the terminal release path never runs) leaves a pod whose owner
// no longer exists, so the collector reaps it instead of orphaning it at full
// CPU (the 3 stranded pods observed 2026-09-12).
func TestObserveSandboxRef_StampsRunOwnerForGC(t *testing.T) {
	const ns = "bmad-squad"
	run := nonTerminalRun("r1", ns, "run-uid-1")
	cl := fake.NewClientBuilder().WithScheme(credWriterScheme(t)).
		WithObjects(run, sandboxPod("sbx-1", ns, "pod-uid-1")).
		WithStatusSubresource(&api.Run{}).
		Build()

	w := NewRunStatusSandboxWriter(cl)
	if err := w.ObserveSandboxRef(context.Background(), "run-uid-1", "sbx-1"); err != nil {
		t.Fatalf("observe: %v", err)
	}

	var pod corev1.Pod
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "sbx-1"}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	ctrl := metav1.GetControllerOf(&pod)
	if ctrl == nil {
		t.Fatalf("sandbox pod carries no controller ownerReference; a Run deleted while non-terminal would orphan it")
	}
	if ctrl.Kind != "Run" || string(ctrl.UID) != "run-uid-1" || ctrl.Name != "r1" {
		t.Errorf("controller ref = %+v, want the owning Run r1/run-uid-1", ctrl)
	}
	if ctrl.APIVersion != api.GroupVersion.String() {
		t.Errorf("ref APIVersion = %q, want %q", ctrl.APIVersion, api.GroupVersion.String())
	}
	// GC on Run deletion, not foreground-blocking the Run's own delete.
	if ctrl.BlockOwnerDeletion == nil || *ctrl.BlockOwnerDeletion {
		t.Errorf("BlockOwnerDeletion = %v, want false (pure GC, no delete ordering)", ctrl.BlockOwnerDeletion)
	}

	// The status surface still lands (M1.2) alongside the owner stamp.
	var got api.Run
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "r1"}, &got); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status.SandboxRef == nil || got.Status.SandboxRef.Name != "sbx-1" {
		t.Errorf("status.sandboxRef = %+v, want sbx-1", got.Status.SandboxRef)
	}
}

// A re-drive/reattach re-observes the same ref; the owner stamp must stay a
// single controller reference, never duplicated or fought over.
func TestObserveSandboxRef_AdoptIsIdempotent(t *testing.T) {
	const ns = "bmad-squad"
	run := nonTerminalRun("r1", ns, "run-uid-1")
	cl := fake.NewClientBuilder().WithScheme(credWriterScheme(t)).
		WithObjects(run, sandboxPod("sbx-1", ns, "pod-uid-1")).
		WithStatusSubresource(&api.Run{}).
		Build()

	w := NewRunStatusSandboxWriter(cl)
	for i := 0; i < 3; i++ {
		if err := w.ObserveSandboxRef(context.Background(), "run-uid-1", "sbx-1"); err != nil {
			t.Fatalf("observe #%d: %v", i, err)
		}
	}

	var pod corev1.Pod
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "sbx-1"}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if len(pod.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %d, want exactly 1 (idempotent re-observe)", len(pod.OwnerReferences))
	}
}

// Fail-safe on the legacy default-namespace path: a namespaced owner and its
// dependent must share a namespace. A cross-namespace ownerReference would make
// GC treat the owner as missing and delete the pod immediately, so the pod is
// left un-adopted (cleanup rides the terminal Release path there).
func TestAdoptSandboxPod_SkipsCrossNamespace(t *testing.T) {
	run := nonTerminalRun("r1", "bmad-squad", "run-uid-1")
	pod := sandboxPod("sbx-1", "default", "pod-uid-1") // NOT the Run's namespace
	cl := fake.NewClientBuilder().WithScheme(credWriterScheme(t)).
		WithObjects(run, pod).Build()

	if err := adoptSandboxPodToRun(context.Background(), cl, run, "sbx-1"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	var got corev1.Pod
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sbx-1"}, &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if len(got.OwnerReferences) != 0 {
		t.Errorf("cross-namespace pod was adopted (%+v); GC would delete it immediately", got.OwnerReferences)
	}
}

// A pod already carrying a controller owner (some other controller) is never
// re-owned — adoption defers rather than fighting for control.
func TestAdoptSandboxPod_RespectsExistingController(t *testing.T) {
	const ns = "bmad-squad"
	run := nonTerminalRun("r1", ns, "run-uid-1")
	pod := sandboxPod("sbx-1", ns, "pod-uid-1")
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "ReplicaSet", Name: "rs-x", UID: "rs-uid",
		Controller: ptrTo(true),
	}}
	cl := fake.NewClientBuilder().WithScheme(credWriterScheme(t)).
		WithObjects(run, pod).Build()

	if err := adoptSandboxPodToRun(context.Background(), cl, run, "sbx-1"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	var got corev1.Pod
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "sbx-1"}, &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Kind != "ReplicaSet" {
		t.Errorf("existing controller clobbered: %+v", got.OwnerReferences)
	}
}
