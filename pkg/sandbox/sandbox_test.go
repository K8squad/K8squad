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

package sandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/credinject"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("add ksquad scheme: %v", err)
	}
	return s
}

func testRun(name, uid string) *api.Run {
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ksquad-team-alpha-38152234",
			UID:       types.UID(uid),
		},
	}
}

// TestBuildSandboxPod (4.2 AC1/AC3): the pod carries a non-empty
// runtimeClassName, lands in the squad namespace as the ksquad-agent SA, is
// named per-Run, and an empty class is a construction failure.
func TestBuildSandboxPod(t *testing.T) {
	run := testRun("r-1", "uid-r-1")
	pod, err := BuildSandboxPod(run, PodSpec{
		Namespace:    run.Namespace,
		TeamName:     "alpha",
		RuntimeClass: ClassGVisor,
		Image:        "ghcr.io/k8squad/opencode:1.0",
	})
	if err != nil {
		t.Fatalf("build pod: %v", err)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != ClassGVisor {
		t.Errorf("runtimeClassName = %v, want gvisor (a pod without it runs on the node default)", pod.Spec.RuntimeClassName)
	}
	if pod.Spec.ServiceAccountName != "ksquad-agent" {
		t.Errorf("serviceAccountName = %q, want ksquad-agent", pod.Spec.ServiceAccountName)
	}
	if pod.Namespace != run.Namespace {
		t.Errorf("namespace = %q, want the squad namespace %q", pod.Namespace, run.Namespace)
	}
	if pod.Labels[LabelRun] != string(run.UID) {
		t.Errorf("run label = %q, want the Run UID", pod.Labels[LabelRun])
	}
	if pod.Name != PodNameFor(run) {
		t.Errorf("pod name %q is not the per-Run derivation %q", pod.Name, PodNameFor(run))
	}

	// Two Runs -> two distinct pods (AC3).
	other, _ := BuildSandboxPod(testRun("r-2", "uid-r-2"), PodSpec{
		Namespace: run.Namespace, TeamName: "alpha", RuntimeClass: ClassGVisor, Image: "img",
	})
	if pod.Name == other.Name {
		t.Errorf("two Runs share one pod %q (AC3 violation)", pod.Name)
	}

	// Empty class fails closed.
	if _, err := BuildSandboxPod(run, PodSpec{Namespace: run.Namespace, TeamName: "alpha", Image: "img"}); err == nil {
		t.Errorf("pod assembled without a RuntimeClass (node-default hole)")
	}
}

// TestBuildSandboxPodInjectsCredentialEnv (story 5.4): the credential
// injection contract's output lands on the agent container as env-by-reference
// — the credential rides a SecretKeyRef, never a literal, so the control plane
// never handles the plaintext. This is the end-to-end verifiable seam the gap
// ticket ISI-2890 flagged as missing (admission checked the Secret existed but
// nothing ever injected it).
func TestBuildSandboxPodInjectsCredentialEnv(t *testing.T) {
	run := testRun("r-cred", "uid-r-cred")
	inj, err := credinject.Inject(api.RuntimeTypeClaudeCode, credinject.ClassHumanSeat, api.SecretRef{Name: "alice-claude"})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	pod, err := BuildSandboxPod(run, PodSpec{
		Namespace:    run.Namespace,
		TeamName:     "alpha",
		RuntimeClass: ClassGVisor,
		Image:        "ghcr.io/k8squad/claude-code:1.0",
		Env:          inj.Env,
	})
	if err != nil {
		t.Fatalf("build pod: %v", err)
	}
	var got *corev1.EnvVar
	for i := range pod.Spec.Containers[0].Env {
		if pod.Spec.Containers[0].Env[i].Name == "CLAUDE_CODE_OAUTH_TOKEN" {
			got = &pod.Spec.Containers[0].Env[i]
		}
	}
	if got == nil {
		t.Fatalf("agent container missing injected CLAUDE_CODE_OAUTH_TOKEN env; got %+v", pod.Spec.Containers[0].Env)
	}
	if got.Value != "" {
		t.Errorf("credential env carries a literal Value %q; must be SecretKeyRef only", got.Value)
	}
	if got.ValueFrom == nil || got.ValueFrom.SecretKeyRef == nil || got.ValueFrom.SecretKeyRef.Name != "alice-claude" {
		t.Errorf("credential env must reference Secret alice-claude by SecretKeyRef, got %+v", got.ValueFrom)
	}
}

// TestTeardownAndReplace (4.5 AC1/AC2/AC3): a completed Run's pod is deleted
// (not reset), deletion of an already-gone pod is a no-op (crash-safe), and
// replenishment mints a FRESH unbound pod.
func TestTeardownAndReplace(t *testing.T) {
	run := testRun("r-1", "uid-r-1")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	pod, err := BuildSandboxPod(run, PodSpec{
		Namespace: run.Namespace, TeamName: "alpha", RuntimeClass: ClassGVisor, Image: "img",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Teardown deletes the pod.
	if err := Teardown(context.Background(), c, run); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	var after corev1.Pod
	err = c.Get(context.Background(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &after)
	if err == nil {
		t.Fatalf("sandbox pod survived teardown (reset-and-reuse path, ADR-006 violation)")
	}

	// Teardown is idempotent / crash-safe on the already-gone path (AC6).
	if err := Teardown(context.Background(), c, run); err != nil {
		t.Fatalf("second teardown must be a no-op: %v", err)
	}

	// Replenish mints a fresh, UNBOUND warm pod (no run label — the claim
	// path stamps it after BindingGuard).
	if err := Replenish(context.Background(), c, PodSpec{
		Namespace: run.Namespace, TeamName: "alpha", RuntimeClass: ClassKata, Image: "img",
	}); err != nil {
		t.Fatalf("replenish: %v", err)
	}
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(run.Namespace)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("warm pods = %d, want 1", len(pods.Items))
	}
	warm := pods.Items[0]
	if warm.Labels[LabelRun] != "" {
		t.Errorf("warm pod must be unbound (no %s label)", LabelRun)
	}
	if warm.Spec.RuntimeClassName == nil || *warm.Spec.RuntimeClassName != ClassKata {
		t.Errorf("warm pod runtimeClassName = %v, want kata (pool keyed by class)", warm.Spec.RuntimeClassName)
	}

	// Replenish without a class fails closed.
	if err := Replenish(context.Background(), c, PodSpec{Namespace: run.Namespace, TeamName: "alpha", Image: "img"}); err == nil {
		t.Errorf("replenished a pod without a RuntimeClass")
	}
}

// TestBuildSandboxPodHardened (Cursor review): the pod carries the
// restricted-PSS hardening set EXPLICITLY — pod-level
// runAsNonRoot+RuntimeDefault seccomp, no SA token automount, no service
// env injection, and a container with no privilege escalation, a read-only
// rootfs (with a writable /tmp emptyDir for legitimate scratch) and all
// capabilities dropped. gVisor covers the kernel boundary; this is what
// keeps the guarantee if the class ever resolves to runc via the
// trusted-dev escape.
func TestBuildSandboxPodHardened(t *testing.T) {
	run := testRun("r-1", "uid-r-1")
	pod, err := BuildSandboxPod(run, PodSpec{
		Namespace: run.Namespace, TeamName: "alpha", RuntimeClass: ClassGVisor, Image: "img",
	})
	if err != nil {
		t.Fatalf("build pod: %v", err)
	}
	sc := pod.Spec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("pod securityContext.runAsNonRoot must be explicitly true, got %+v", sc)
	}
	if sc == nil || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccompProfile must be RuntimeDefault, got %+v", sc)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("pod automountServiceAccountToken must be explicitly false")
	}
	if pod.Spec.EnableServiceLinks == nil || *pod.Spec.EnableServiceLinks {
		t.Errorf("pod enableServiceLinks must be explicitly false")
	}
	csc := pod.Spec.Containers[0].SecurityContext
	if csc == nil || csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Errorf("container allowPrivilegeEscalation must be false, got %+v", csc)
	}
	if csc == nil || csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
		t.Errorf("container readOnlyRootFilesystem must be true, got %+v", csc)
	}
	if csc == nil || csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container capabilities must drop ALL, got %+v", csc)
	}
	var sawTmp bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "tmp" && m.MountPath == "/tmp" {
			sawTmp = true
		}
	}
	if !sawTmp {
		t.Errorf("read-only rootfs without a writable /tmp mount breaks agent runtimes needing scratch")
	}
}

// TestBuildSandboxPodRequiresRunUID (Cursor review): a Run without a UID
// fails closed — the UID is what the §9.3 binding and the collision-safe
// pod name derive from, and an empty one would emit an unbound-looking pod
// whose name collides with every other empty-UID Run of the same name.
func TestBuildSandboxPodRequiresRunUID(t *testing.T) {
	run := testRun("r-1", "")
	_, err := BuildSandboxPod(run, PodSpec{
		Namespace: run.Namespace, TeamName: "alpha", RuntimeClass: ClassGVisor, Image: "img",
	})
	if err == nil {
		t.Fatalf("pod assembled for a UID-less Run (fail-open shape)")
	}
	if !IsPolicyError(err) {
		t.Errorf("error is not a PolicyError: %v", err)
	}
}

// TestBindingGuard (4.5 AC2 — the §9.3 absolute): a pod bound to one Run is
// never handed to another.
func TestBindingGuard(t *testing.T) {
	run := testRun("r-1", "uid-r-1")
	other := testRun("r-2", "uid-r-2")

	bound, _ := BuildSandboxPod(run, PodSpec{
		Namespace: run.Namespace, TeamName: "alpha", RuntimeClass: ClassGVisor, Image: "img",
	})
	if err := BindingGuard(bound, run); err != nil {
		t.Errorf("own pod rejected: %v", err)
	}
	if err := BindingGuard(bound, other); err == nil {
		t.Errorf("pod bound to %s handed to %s (a sandbox is never reused across Runs)", run.UID, other.UID)
	}

	// A UID-less Run must not be handed any pod — including unbound-looking
	// ones (Cursor review: the empty-UID fail-open shape).
	if err := BindingGuard(bound, testRun("r-3", "")); err == nil {
		t.Errorf("UID-less Run admitted to claim a sandbox (binding identity is UID-scoped)")
	}
}

// TestWorkspaceMountsRejectHostilePVCKey (Cursor review): the
// caller-supplied pvcKey is the one component that can traverse — the
// JOINED subPaths are validated, so a hostile key fails closed instead of
// flowing straight into the emitted subPath.
func TestWorkspaceMountsRejectHostilePVCKey(t *testing.T) {
	alice := api.PrincipalRef("alice@corp.com")
	for _, key := range []string{"../../other-principal", "/etc", "a/../../..", ".."} {
		mounts, err := WorkspaceVolumeMounts("workspace-pvc", key, alice)
		if err == nil {
			t.Errorf("hostile pvcKey %q admitted: %+v", key, mounts)
		} else if !IsPolicyError(err) {
			t.Errorf("pvcKey %q: error is not a PolicyError: %v", key, err)
		}
	}
	// The benign shapes still pass: empty (dedicated PVC) and a plain
	// relative fragment.
	for _, key := range []string{"", "projects/widget"} {
		if _, err := WorkspaceVolumeMounts("workspace-pvc", key, alice); err != nil {
			t.Errorf("benign pvcKey %q rejected: %v", key, err)
		}
	}
}

// TestPrincipalPartitionScoping (4.5 AC4/AC5): partitions are deterministic
// and collision-safe per principal; mounts only ever carry the Run's own
// principal's subpath; traversal-shaped partitions fail closed.
func TestPrincipalPartitionScoping(t *testing.T) {
	alice := api.PrincipalRef("alice@corp.com")
	bob := api.PrincipalRef("bob@corp.com")

	pa, pb := PrincipalPartition(alice), PrincipalPartition(bob)
	if pa == pb {
		t.Fatalf("distinct principals share partition %q (D7 exfil hole)", pa)
	}
	if PrincipalPartition(alice) != pa {
		t.Errorf("partition not deterministic")
	}
	for _, p := range []string{pa, pb} {
		if err := ValidatePartition(p); err != nil {
			t.Errorf("derived partition %q invalid: %v", p, err)
		}
	}

	// Mounts are scoped to the Run's own principal (AC4): every subPath
	// sits under alice's partition only.
	mounts, err := WorkspaceVolumeMounts("workspace-pvc", "", alice)
	if err != nil {
		t.Fatalf("mounts: %v", err)
	}
	for _, m := range mounts {
		if !containsSubPath(m.SubPath, pa) && m.Name == "workspace-cache" {
			t.Errorf("cache mount subPath %q not scoped to the principal partition %q", m.SubPath, pa)
		}
		if m.Name == "workspace-cache" && m.SubPath != pa {
			t.Errorf("cache subPath = %q, want exactly %q", m.SubPath, pa)
		}
	}

	// Traversal-shaped partitions fail closed (defense in depth).
	for _, bad := range []string{"../escape", "/abs", "cache/../x", "other-root/x", "cache/.."} {
		if err := ValidatePartition(bad); err == nil {
			t.Errorf("partition %q admitted (traversal)", bad)
		}
	}

	// Hostile principal ids are normalized safely.
	weird := api.PrincipalRef("../../etc/passwd")
	pw := PrincipalPartition(weird)
	if err := ValidatePartition(pw); err != nil {
		t.Errorf("hostile principal partition %q invalid: %v", pw, err)
	}
}

func containsSubPath(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(haystack) > len(needle) &&
		(haystack[:len(needle)] == needle))
}

// TestWorkspaceMountsFailClosedOnEmptyPrincipal (PR #89 follow-up F6): an
// empty principal must not fall back to the literal shared "principal"
// partition — every identity-less caller would commingle files there once
// story 4.3 wires callers. Fail closed instead.
func TestWorkspaceMountsFailClosedOnEmptyPrincipal(t *testing.T) {
	mounts, err := WorkspaceVolumeMounts("workspace-pvc", "", "")
	if err == nil {
		t.Fatalf("empty principal admitted into the shared fallback partition: %+v", mounts)
	}
	if !IsPolicyError(err) {
		t.Errorf("error is not a PolicyError: %v", err)
	}
	// A non-empty principal still passes (and never shares the fallback).
	if _, err := WorkspaceVolumeMounts("workspace-pvc", "", "alice@corp.com"); err != nil {
		t.Fatalf("non-empty principal rejected: %v", err)
	}
}

// TestSandboxScratchVolumeNameGuard (PR #89 follow-up F9): caller-supplied
// mounts or volumes already named "tmp" fail closed — appending the scratch
// /tmp onto them would emit duplicate volume names, an invalid pod spec
// that only blows up at admission.
func TestSandboxScratchVolumeNameGuard(t *testing.T) {
	run := testRun("r-1", "uid-r-1")

	collideMount := PodSpec{
		Namespace: run.Namespace, TeamName: "alpha", RuntimeClass: ClassGVisor, Image: "img",
		Mounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/scratch"}},
	}
	if _, err := BuildSandboxPod(run, collideMount); err == nil {
		t.Errorf("mount named %q admitted (duplicate volume names)", "tmp")
	} else if !IsPolicyError(err) {
		t.Errorf("mount collision error is not a PolicyError: %v", err)
	}

	collideVolume := PodSpec{
		Namespace: run.Namespace, TeamName: "alpha", RuntimeClass: ClassGVisor, Image: "img",
		Volumes: []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
	}
	if _, err := BuildSandboxPod(run, collideVolume); err == nil {
		t.Errorf("volume named %q admitted (duplicate volume names)", "tmp")
	} else if !IsPolicyError(err) {
		t.Errorf("volume collision error is not a PolicyError: %v", err)
	}

	// Distinct names still assemble fine (and the scratch /tmp is added).
	pod, err := BuildSandboxPod(run, PodSpec{
		Namespace: run.Namespace, TeamName: "alpha", RuntimeClass: ClassGVisor, Image: "img",
		Mounts:  []corev1.VolumeMount{{Name: "workspace-cache", MountPath: "/workspace/cache"}},
		Volumes: []corev1.Volume{{Name: "workspace-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
	})
	if err != nil {
		t.Fatalf("benign spec rejected: %v", err)
	}
	seen := map[string]int{}
	for _, v := range pod.Spec.Volumes {
		seen[v.Name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("duplicate volume name %q in emitted pod spec: %+v", name, pod.Spec.Volumes)
		}
	}
	if seen["tmp"] != 1 {
		t.Errorf("scratch /tmp volume missing from emitted pod spec: %+v", pod.Spec.Volumes)
	}
}
