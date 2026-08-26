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

package workspace

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// DB-free unit tests for the workspace PVC manager: pure builders/helpers plus the
// fake-client-backed Ensure/Get/Delete/Reconcile flows. No Postgres — runs in the
// ci.yml unit lane and lifts the gated coverage number (ISI-3213).

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ksquadv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add ksquad scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return s
}

func newRun() *ksquadv1alpha1.Run {
	return &ksquadv1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-1",
			Namespace: "default",
			UID:       types.UID("uid-run-1"),
		},
	}
}

func newManager(t *testing.T, objs ...runtime.Object) *Manager {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(testScheme(t))
	for _, o := range objs {
		b = b.WithRuntimeObjects(o)
	}
	return NewWorkspaceManager(b.Build())
}

func TestEnsureWorkspaceCreatesPVC(t *testing.T) {
	run := newRun()
	m := newManager(t, run)

	pvc, err := m.EnsureWorkspace(context.Background(), run)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if pvc.Name != "workspace-run-1" {
		t.Errorf("pvc name = %q", pvc.Name)
	}
	if pvc.Labels[LabelWorkspace] != "true" || pvc.Labels[LabelRun] != run.Name {
		t.Errorf("labels = %v", pvc.Labels)
	}
	if len(pvc.OwnerReferences) != 1 || pvc.OwnerReferences[0].UID != run.UID {
		t.Errorf("owner reference = %+v", pvc.OwnerReferences)
	}
	if pvc.OwnerReferences[0].Kind != "Run" {
		t.Errorf("owner kind = %q, want Run", pvc.OwnerReferences[0].Kind)
	}
	// Storage request + class + access mode.
	want := resource.MustParse(WorkspacePVCSize)
	got := pvc.Spec.Resources.Requests["storage"]
	if got.Cmp(want) != 0 {
		t.Errorf("storage request = %s, want %s", got.String(), want.String())
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != WorkspaceStorageClass {
		t.Errorf("storage class = %v", pvc.Spec.StorageClassName)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("access modes = %v", pvc.Spec.AccessModes)
	}
}

func TestEnsureWorkspaceIdempotent(t *testing.T) {
	run := newRun()
	m := newManager(t, run)
	ctx := context.Background()

	first, err := m.EnsureWorkspace(ctx, run)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, err := m.EnsureWorkspace(ctx, run)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	// Second call must return the existing PVC unchanged (same ResourceVersion).
	if second.ResourceVersion != first.ResourceVersion {
		t.Errorf("idempotent ensure changed ResourceVersion %s -> %s", first.ResourceVersion, second.ResourceVersion)
	}
}

func TestEnsureWorkspaceCreateError(t *testing.T) {
	run := newRun()
	boom := errors.NewInternalError(context.DeadlineExceeded)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRuntimeObjects(run).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return boom
			},
		}).Build()
	m := NewWorkspaceManager(c)
	if _, err := m.EnsureWorkspace(context.Background(), run); err == nil {
		t.Error("create error should propagate")
	}
}

func TestEnsureWorkspaceGetError(t *testing.T) {
	run := newRun()
	boom := errors.NewServiceUnavailable("apiserver down")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRuntimeObjects(run).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	m := NewWorkspaceManager(c)
	if _, err := m.EnsureWorkspace(context.Background(), run); err == nil {
		t.Error("non-NotFound get error should propagate")
	}
}

func TestGetWorkspace(t *testing.T) {
	run := newRun()
	m := newManager(t, run)
	ctx := context.Background()

	// Missing → error.
	if _, err := m.GetWorkspace(ctx, run); err == nil {
		t.Error("expected error getting non-existent workspace")
	}

	if _, err := m.EnsureWorkspace(ctx, run); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	pvc, err := m.GetWorkspace(ctx, run)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pvc.Name != "workspace-run-1" {
		t.Errorf("pvc name = %q", pvc.Name)
	}
}

func TestDeleteWorkspace(t *testing.T) {
	run := newRun()
	m := newManager(t, run)
	ctx := context.Background()

	// Deleting an absent workspace is a no-op.
	if err := m.DeleteWorkspace(ctx, run); err != nil {
		t.Errorf("delete of absent workspace should be no-op: %v", err)
	}

	if _, err := m.EnsureWorkspace(ctx, run); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := m.DeleteWorkspace(ctx, run); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.GetWorkspace(ctx, run); err == nil {
		t.Error("workspace should be gone after delete")
	}
}

func TestReconcileCreatesWorkspace(t *testing.T) {
	run := newRun()
	m := newManager(t, run)

	res, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: run.Name, Namespace: run.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("unexpected result: %+v", res)
	}
	if _, err := m.GetWorkspace(context.Background(), run); err != nil {
		t.Errorf("workspace should exist post-reconcile: %v", err)
	}
}

func TestReconcileMissingRunNoOp(t *testing.T) {
	m := newManager(t)
	if _, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ghost", Namespace: "default"},
	}); err != nil {
		t.Errorf("reconcile of absent run should be a no-op: %v", err)
	}
}

func TestReconcileDeletingRunNoOp(t *testing.T) {
	run := newRun()
	now := metav1.Now()
	run.DeletionTimestamp = &now
	run.Finalizers = []string{"k8squad.io/test"}
	m := newManager(t, run)

	if _, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: run.Name, Namespace: run.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := m.GetWorkspace(context.Background(), run); err == nil {
		t.Error("no workspace should be created for a deleting run")
	}
}

func TestVolumeMountAndVolume(t *testing.T) {
	vm := VolumeMount("run-1")
	if vm.Name != "workspace" || vm.MountPath != "/workspace" || vm.ReadOnly {
		t.Errorf("volume mount = %+v", vm)
	}
	v := Volume("run-1")
	if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "workspace-run-1" {
		t.Errorf("volume = %+v", v)
	}
}

func TestIsWorkspaceOwned(t *testing.T) {
	cases := []struct {
		name string
		pvc  *corev1.PersistentVolumeClaim
		want bool
	}{
		{"nil", nil, false},
		{"no labels", &corev1.PersistentVolumeClaim{}, false},
		{"missing run label", &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelWorkspace: "true"}}}, false},
		{"both labels", &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelWorkspace: "true", LabelRun: "run-1"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsWorkspaceOwned(tc.pvc); got != tc.want {
				t.Errorf("IsWorkspaceOwned = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetRunForWorkspace(t *testing.T) {
	if _, ok := GetRunForWorkspace(nil); ok {
		t.Error("nil pvc should return ok=false")
	}
	if _, ok := GetRunForWorkspace(&corev1.PersistentVolumeClaim{}); ok {
		t.Error("labelless pvc should return ok=false")
	}
	name, ok := GetRunForWorkspace(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelRun: "run-9"}},
	})
	if !ok || name != "run-9" {
		t.Errorf("GetRunForWorkspace = %q, %v", name, ok)
	}
}

func TestPtrTo(t *testing.T) {
	if got := ptrTo(true); got == nil || !*got {
		t.Fatalf("ptrTo(true) = %v", got)
	}
}
