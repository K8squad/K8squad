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
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// TestSupervisorURLPodIPRace covers the ISI-4441 bind/readiness race
// classification in supervisorURL: a bound pod with no IP is a benign,
// requeue-worthy errSandboxPending while it is young, and a loud (span-worthy)
// error once it has aged past podIPReadyDeadline.
func TestSupervisorURLPodIPRace(t *testing.T) {
	const runUID = "eeeeeeee-2222-4444-8888-eeeeeeeeeeee"
	created := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	newDispatch := func(t *testing.T, podIP string, now time.Time) *operatorDispatch {
		t.Helper()
		run := &api.Run{
			ObjectMeta: metav1.ObjectMeta{Name: "run-race", Namespace: "team-a", UID: types.UID(runUID)},
			Spec: api.RunSpec{
				TeamRef:     api.ObjectRef{Name: "team-a"},
				ProjectRef:  api.ObjectRef{Name: "proj-1"},
				WorkItemRef: "77777777-8888-9999-aaaa-bbbbbbbbbbbb",
			},
			Status: api.RunStatus{
				SandboxRef: &api.ObjectRef{Name: "sandbox-race", Namespace: "team-a"},
			},
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "sandbox-race",
				Namespace:         "team-a",
				CreationTimestamp: metav1.NewTime(created),
			},
			Status: corev1.PodStatus{PodIP: podIP},
		}
		cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).WithObjects(run, pod).Build()
		return &operatorDispatch{
			cfg:     OperatorDispatchConfig{Client: cl},
			shimBin: "shim",
			now:     func() time.Time { return now },
		}
	}

	t.Run("networked pod resolves to a URL", func(t *testing.T) {
		d := newDispatch(t, "10.1.2.3", created.Add(5*time.Second))
		url, err := d.supervisorURL(context.Background(), runUID)
		if err != nil {
			t.Fatalf("supervisorURL: %v", err)
		}
		if url != "http://10.1.2.3:8080/task" {
			t.Errorf("url=%q, want the pod IP endpoint", url)
		}
	})

	t.Run("young IP-less pod is a benign errSandboxPending", func(t *testing.T) {
		d := newDispatch(t, "", created.Add(10*time.Second)) // well under the deadline
		url, err := d.supervisorURL(context.Background(), runUID)
		if url != "" {
			t.Errorf("url=%q, want empty on the pending race", url)
		}
		if !errors.Is(err, errSandboxPending) {
			t.Fatalf("err=%v, want it to wrap errSandboxPending so the driver requeues quietly", err)
		}
	})

	t.Run("stale IP-less pod escalates to a loud error", func(t *testing.T) {
		d := newDispatch(t, "", created.Add(podIPReadyDeadline+time.Second))
		url, err := d.supervisorURL(context.Background(), runUID)
		if url != "" {
			t.Errorf("url=%q, want empty when the pod never got an IP", url)
		}
		if err == nil {
			t.Fatal("want a loud error once the pod ages past the readiness deadline")
		}
		if errors.Is(err, errSandboxPending) {
			t.Error("a pod past the deadline must NOT be classified as the benign race (it should surface on the span)")
		}
	})
}
