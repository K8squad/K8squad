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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return s
}

func validSpec() Spec {
	return Spec{
		RunID:          "run-42",
		ProjectPVCName: "proj-pvc-7",
		CommitSHA:      "deadbeefcafe",
		ReaderSAName:   "run-42-reader",
	}
}

// TestNewLauncher_FlagOffDegrades: with the flag off (the default), NewLauncher hands back a
// DisabledLauncher whose Launch is ErrDisabled — the browser degrades to snapshot-only and this
// story never blocks 8.7e.
func TestNewLauncher_FlagOffDegrades(t *testing.T) {
	l := NewLauncher(Config{Enabled: false}, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	if _, ok := l.(DisabledLauncher); !ok {
		t.Fatalf("flag off: got %T, want DisabledLauncher", l)
	}
	if _, err := l.Launch(context.Background(), validSpec()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled Launch err = %v, want ErrDisabled", err)
	}
	if err := l.TearDown(context.Background(), Handle{}); err != nil {
		t.Fatalf("disabled TearDown err = %v, want nil", err)
	}
}

// TestNewLauncher_FlagOnNilClientDegrades: fail-safe — the flag on but no client still degrades
// rather than handing back a launcher that would nil-panic on first read.
func TestNewLauncher_FlagOnNilClientDegrades(t *testing.T) {
	if l := NewLauncher(Config{Enabled: true}, nil); !isDisabled(l) {
		t.Fatalf("flag on + nil client: got %T, want DisabledLauncher (fail safe)", l)
	}
}

func isDisabled(l Launcher) bool { _, ok := l.(DisabledLauncher); return ok }

// TestKubeLauncher_LaunchCreatesPodAndCounts: an enabled launcher creates a real reader pod and
// increments the alert-worthy launch counter.
func TestKubeLauncher_LaunchCreatesPodAndCounts(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	l := NewLauncher(Config{Enabled: true, ReaderImage: "ghcr.io/k8squad/build-reader:v1"}, c)
	if isDisabled(l) {
		t.Fatal("flag on + client: got DisabledLauncher, want KubeLauncher")
	}
	before := launchCount()

	h, err := l.Launch(context.Background(), validSpec())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if h.PodName != PodName("run-42") {
		t.Errorf("Handle.PodName = %q, want %q", h.PodName, PodName("run-42"))
	}
	if got := launchCount() - before; got != 1 {
		t.Errorf("launch counter delta = %v, want 1", got)
	}

	// The pod really exists in the cluster.
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: h.PodName, Namespace: h.Namespace}, &pod); err != nil {
		t.Fatalf("reader pod not created: %v", err)
	}

	// TearDown removes it, and a second TearDown is idempotent (not-found ⇒ success).
	if err := l.TearDown(context.Background(), h); err != nil {
		t.Fatalf("TearDown: %v", err)
	}
	if err := l.TearDown(context.Background(), h); err != nil {
		t.Fatalf("idempotent TearDown: %v", err)
	}
}

// TestKubeLauncher_RejectsBadSpec: an under-specified request fails closed before any pod is created.
func TestKubeLauncher_RejectsBadSpec(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	l := NewLauncher(Config{Enabled: true, ReaderImage: "img"}, c)
	for _, tc := range []struct {
		name string
		spec Spec
	}{
		{"no run", Spec{ProjectPVCName: "p", CommitSHA: "c", ReaderSAName: "sa"}},
		{"no pvc", Spec{RunID: "r", CommitSHA: "c", ReaderSAName: "sa"}},
		{"no commit", Spec{RunID: "r", ProjectPVCName: "p", ReaderSAName: "sa"}},
		{"no sa", Spec{RunID: "r", ProjectPVCName: "p", CommitSHA: "c"}},
	} {
		if _, err := l.Launch(context.Background(), tc.spec); err == nil {
			t.Errorf("%s: Launch accepted an invalid spec, want error", tc.name)
		}
	}
}

// TestBuildPod_LeastPrivilegeInvariants asserts the security-critical fields the 8.7f AC mandates:
// RO PVC mount, the Run's own SA, non-root, read-only rootfs, no priv-escalation, and an idle
// self-terminate deadline. These are structural — a regression flips a boolean here, not in prose.
func TestBuildPod_LeastPrivilegeInvariants(t *testing.T) {
	pod := BuildPod(validSpec(), Config{Enabled: true, ReaderImage: "img"})

	if pod.Spec.ServiceAccountName != "run-42-reader" {
		t.Errorf("ServiceAccountName = %q, want the Run's own reader SA", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != defaultActiveDeadlineSec {
		t.Errorf("ActiveDeadlineSeconds = %v, want %d (idle self-terminate)", pod.Spec.ActiveDeadlineSeconds, defaultActiveDeadlineSec)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %v, want Never (short-lived)", pod.Spec.RestartPolicy)
	}
	if sc := pod.Spec.SecurityContext; sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("pod SecurityContext must set RunAsNonRoot=true")
	}

	// The PVC volume is ReadOnly at both the volume source and the mount.
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatalf("want exactly one PVC volume, got %+v", pod.Spec.Volumes)
	}
	vol := pod.Spec.Volumes[0].PersistentVolumeClaim
	if vol.ClaimName != "proj-pvc-7" || !vol.ReadOnly {
		t.Errorf("PVC volume = %q ReadOnly=%v, want proj-pvc-7 ReadOnly=true", vol.ClaimName, vol.ReadOnly)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("want one container, got %d", len(pod.Spec.Containers))
	}
	ctr := pod.Spec.Containers[0]
	if len(ctr.VolumeMounts) != 1 || !ctr.VolumeMounts[0].ReadOnly {
		t.Errorf("container mount must be ReadOnly, got %+v", ctr.VolumeMounts)
	}
	if csc := ctr.SecurityContext; csc == nil ||
		csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem ||
		csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Error("container must set ReadOnlyRootFilesystem=true and AllowPrivilegeEscalation=false")
	}
}

// Compile-time proof both launchers satisfy the seam.
var _ Launcher = DisabledLauncher{}
var _ Launcher = (*KubeLauncher)(nil)
var _ client.Client = (client.Client)(nil)
