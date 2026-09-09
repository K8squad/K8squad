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

package readerpod

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestReaper_ReapHandle_TearsDownAndMeters: ReapHandle removes the reader (pod+service) and records
// exactly one reap.
func TestReaper_ReapHandle_TearsDownAndMeters(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	l := NewLauncher(Config{Enabled: true, ReaderImage: "img"}, c)
	h, err := l.Launch(context.Background(), validSpec())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	r := NewReaper(c, l, DefaultNamespace, 0)

	before := reapCount()
	if err := r.ReapHandle(context.Background(), h); err != nil {
		t.Fatalf("ReapHandle: %v", err)
	}
	if got := reapCount() - before; got != 1 {
		t.Errorf("reap counter delta = %v, want 1", got)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: h.PodName, Namespace: h.Namespace}, &pod); err == nil {
		t.Error("pod still present after ReapHandle")
	}

	// A zero handle is a no-op and does NOT meter.
	before = reapCount()
	if err := r.ReapHandle(context.Background(), Handle{}); err != nil {
		t.Fatalf("ReapHandle(zero): %v", err)
	}
	if got := reapCount() - before; got != 0 {
		t.Errorf("zero-handle reap counted %v, want 0", got)
	}
}

// TestReaper_ReapIdle_AgeCutoff: the orphan sweep reaps a reader older than maxLife and leaves a
// fresh one alone.
func TestReaper_ReapIdle_AgeCutoff(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	old := readerPodFor("run-old", now.Add(-20*time.Minute))
	fresh := readerPodFor("run-new", now.Add(-1*time.Minute))
	oldSvc := BuildService(Spec{RunID: "run-old"}, Config{})
	freshSvc := BuildService(Spec{RunID: "run-new"}, Config{})

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(old, fresh, oldSvc, freshSvc).Build()
	l := NewLauncher(Config{Enabled: true, ReaderImage: "img"}, c)
	r := NewReaper(c, l, DefaultNamespace, DefaultMaxLifetime)
	r.now = func() time.Time { return now }

	before := reapCount()
	n, err := r.ReapIdle(context.Background())
	if err != nil {
		t.Fatalf("ReapIdle: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d, want 1 (only the aged reader)", n)
	}
	if got := reapCount() - before; got != 1 {
		t.Errorf("reap counter delta = %v, want 1", got)
	}

	// The aged reader and its service are gone; the fresh one survives.
	var p corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: PodName("run-old"), Namespace: DefaultNamespace}, &p); err == nil {
		t.Error("aged reader pod survived the sweep")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: ServiceName("run-old"), Namespace: DefaultNamespace}, &corev1.Service{}); err == nil {
		t.Error("aged reader service survived the sweep")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: PodName("run-new"), Namespace: DefaultNamespace}, &p); err != nil {
		t.Errorf("fresh reader pod was reaped: %v", err)
	}
}

// readerPodFor builds a reader pod object with a chosen creation time and the reader app label, so
// the sweep can find it and judge its age.
func readerPodFor(runID string, created time.Time) *corev1.Pod {
	p := BuildPod(Spec{RunID: runID, ProjectPVCName: "pvc", CommitSHA: "c", ReaderSAName: "sa"}, Config{ReaderImage: "img"})
	p.CreationTimestamp = metav1.NewTime(created)
	return p
}
