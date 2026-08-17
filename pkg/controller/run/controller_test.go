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
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

type fakeSource struct {
	step  reconcile.Step
	found bool
	err   error
}

func (f fakeSource) StepForRun(context.Context, string) (reconcile.Step, bool, error) {
	return f.step, f.found, f.err
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func newRun() *api.Run {
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "run-1",
			Namespace:  "default",
			UID:        types.UID("uid-run-1"),
			Generation: 4,
		},
	}
}

func reconcileOnce(t *testing.T, c client.Client, src StepSource) (ctrl.Result, error) {
	t.Helper()
	r := &Reconciler{Client: c, Source: src, Now: func() metav1.Time { return fixedNow }}
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "default"},
	})
}

func TestReconcileProjectsDurableStep(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()

	if _, err := reconcileOnce(t, c, fakeSource{step: reconcile.StepRunning, found: true}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got api.Run
	if err := c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != api.RunPhaseRunning {
		t.Errorf("Phase = %q, want Running", got.Status.Phase)
	}
	if got.Status.ObservedGeneration != 4 {
		t.Errorf("ObservedGeneration = %d, want 4", got.Status.ObservedGeneration)
	}
	if meta.FindStatusCondition(got.Status.Conditions, ConditionReady) == nil {
		t.Errorf("Ready condition not written")
	}
}

func TestReconcileNoClaimRowIsPending(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()

	// found=false: the Run is admitted but not yet enrolled in coord.
	if _, err := reconcileOnce(t, c, fakeSource{found: false}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got api.Run
	_ = c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &got)
	if got.Status.Phase != api.RunPhasePending {
		t.Errorf("Phase = %q, want Pending", got.Status.Phase)
	}
}

func TestReconcileSourceErrorRequeues(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()

	_, err := reconcileOnce(t, c, fakeSource{err: errors.New("db stall")})
	if err == nil {
		t.Fatalf("expected error to trigger requeue, got nil")
	}
}

func TestReconcileMissingRunIsNoop(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithStatusSubresource(&api.Run{}).Build()

	res, err := reconcileOnce(t, c, fakeSource{step: reconcile.StepRunning, found: true})
	if err != nil {
		t.Fatalf("missing Run should not error, got %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("missing Run should not requeue, got %+v", res)
	}
}

// TestReconcileIsIdempotent proves a second pass with the same durable step does
// not rewrite status (no ResourceVersion churn), which keeps a hot requeue loop
// from hammering the API server.
func TestReconcileIsIdempotent(t *testing.T) {
	run := newRun()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(run).WithStatusSubresource(&api.Run{}).Build()
	src := fakeSource{step: reconcile.StepSucceeded, found: true}

	if _, err := reconcileOnce(t, c, src); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var afterFirst api.Run
	_ = c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &afterFirst)

	if _, err := reconcileOnce(t, c, src); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	var afterSecond api.Run
	_ = c.Get(context.Background(), types.NamespacedName{Name: "run-1", Namespace: "default"}, &afterSecond)

	if afterFirst.ResourceVersion != afterSecond.ResourceVersion {
		t.Errorf("status rewritten on no-op reconcile: rv %s -> %s",
			afterFirst.ResourceVersion, afterSecond.ResourceVersion)
	}
	if cond := meta.FindStatusCondition(afterSecond.Status.Conditions, ConditionReady); cond == nil ||
		cond.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True for Succeeded, got %+v", cond)
	}
}
