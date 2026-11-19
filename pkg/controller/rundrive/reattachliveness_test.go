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
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// fakeRunListReader serves only the RunList List call (client.Reader satisfied
// by embedding; Get is never exercised on this seam).
type fakeRunListReader struct {
	client.Reader
	runs []api.Run
	err  error
}

func (f *fakeRunListReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if f.err != nil {
		return f.err
	}
	out := list.(*api.RunList)
	out.Items = append(out.Items, f.runs...)
	return nil
}

// fakePodListReader serves only the PodList List call.
type fakePodListReader struct {
	client.Reader
	pods []corev1.Pod
	err  error
}

func (f *fakePodListReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if f.err != nil {
		return f.err
	}
	out := list.(*corev1.PodList)
	out.Items = append(out.Items, f.pods...)
	return nil
}

func boundRun(uid string) api.Run {
	run := api.Run{}
	run.UID = types.UID(uid)
	run.Status.SandboxRef = &api.ObjectRef{Name: "pod-" + uid, Namespace: "default"}
	return run
}

func livePod(name string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

// ISI-7543: DeadSandbox condemns ONLY positive evidence — a bound sandbox run
// with zero live pods. Unbound (mesh/stdio) runs, missing CRs, and unwired
// seams are fail-safe not-dead; list errors defer to the caller.
func TestSandboxLivenessDeadSandbox(t *testing.T) {
	terminating := livePod("pod-run-term")
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	cases := []struct {
		name string
		uid  string
		runs []api.Run
		pods []corev1.Pod
		want bool
	}{
		{name: "bound run, zero pods", uid: "run-1", runs: []api.Run{boundRun("run-1")}, want: true},
		{name: "bound run, terminating pod only", uid: "run-term", runs: []api.Run{boundRun("run-term")}, pods: []corev1.Pod{terminating}, want: true},
		{name: "bound run, live pod", uid: "run-2", runs: []api.Run{boundRun("run-2")}, pods: []corev1.Pod{livePod("pod-run-2")}},
		{name: "unbound run (mesh/stdio)", uid: "run-mesh", runs: []api.Run{api.Run{}}},
		{name: "run CR gone", uid: "run-gone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "unbound run (mesh/stdio)" {
				c.runs[0].UID = types.UID(c.uid)
			}
			s := &SandboxLiveness{
				Runs: &fakeRunListReader{runs: c.runs},
				Pods: &LiveRuns{Reader: &fakePodListReader{pods: c.pods}},
			}
			got, err := s.DeadSandbox(context.Background(), c.uid)
			if err != nil {
				t.Fatalf("DeadSandbox err = %v, want nil", err)
			}
			if got != c.want {
				t.Fatalf("DeadSandbox = %v, want %v", got, c.want)
			}
		})
	}
}

// The pod leg is never consulted for an unbound run: a pod-list error there
// must not surface (and must not condemn).
func TestSandboxLivenessUnboundNeverConsultsPods(t *testing.T) {
	run := api.Run{}
	run.UID = types.UID("run-mesh")
	s := &SandboxLiveness{
		Runs: &fakeRunListReader{runs: []api.Run{run}},
		Pods: &LiveRuns{Reader: &fakePodListReader{err: errors.New("pods must not be listed for an unbound run")}},
	}
	got, err := s.DeadSandbox(context.Background(), "run-mesh")
	if err != nil || got {
		t.Fatalf("DeadSandbox = (%v, %v), want (false, nil)", got, err)
	}
}

// A Run-list failure defers (error out), never a blind verdict.
func TestSandboxLivenessRunListErrorDefers(t *testing.T) {
	s := &SandboxLiveness{
		Runs: &fakeRunListReader{err: errors.New("list boom")},
		Pods: &LiveRuns{Reader: &fakePodListReader{}},
	}
	if _, err := s.DeadSandbox(context.Background(), "run-1"); err == nil {
		t.Fatal("run-list error must be returned, not swallowed")
	}
}

// An unwired seam is fail-safe not-dead (nil receiver, nil readers, empty id).
func TestSandboxLivenessUnwiredFailSafe(t *testing.T) {
	var nilProbe *SandboxLiveness
	if got, err := nilProbe.DeadSandbox(context.Background(), "run-1"); got || err != nil {
		t.Fatalf("nil probe = (%v, %v), want (false, nil)", got, err)
	}
	s := &SandboxLiveness{}
	if got, err := s.DeadSandbox(context.Background(), "run-1"); got || err != nil {
		t.Fatalf("nil readers = (%v, %v), want (false, nil)", got, err)
	}
	wired := &SandboxLiveness{Runs: &fakeRunListReader{}, Pods: &LiveRuns{Reader: &fakePodListReader{}}}
	if got, err := wired.DeadSandbox(context.Background(), ""); got || err != nil {
		t.Fatalf("empty runID = (%v, %v), want (false, nil)", got, err)
	}
}
