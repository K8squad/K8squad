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

package warmpool_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/pkg/warmpool"
)

// ISI-4291 — restart reconciliation. The pool's inventory is in-memory
// only, so an operator restart used to orphan the ENTIRE warm pool and
// boot a full replacement set. The scenarios here pin the ticket's
// verification directly:
//
//	A1 adopt-ready-reap-stale   5 warm pods adopted, stale orphan reaped,
//                               run-owned pod left alone
//	A2 no-replacement-boot      post-adoption Tick boots NOTHING (the
//                               adopted 5 count as live), warm Bind pops
//                               an adopted pod with zero Boot calls
//	A3 reap-nonready-unmanaged  a crashed generation's in-flight boot and
//                               a config-drifted key are both reaped
//	A4 adopt unit               idempotent, never run-bound, claimable

// newFakeKube builds the fake kube cluster (scheme + client) the provisioner
// and AdoptOrReap share.
func newFakeKube(t *testing.T) (client.Client, *warmpool.KubeProvisioner) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&corev1.Pod{}).Build()
	return c, warmpool.NewKubeProvisioner(c, "", "")
}

// bootWarmPod boots a pod through the REAL provisioner (production-shaped
// annotations), then stamps its Ready condition and a deterministic
// creation instant (age minutes: older = warmer — FIFO determinism).
func bootWarmPod(t *testing.T, ctx context.Context, c client.Client, p *warmpool.KubeProvisioner, key warmpool.PoolKey, name string, ageMinutes int, ready bool) {
	t.Helper()
	if err := p.Boot(ctx, key, name, warmpool.BootWarm); err != nil {
		t.Fatalf("boot %s: %v", name, err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: name}, pod); err != nil {
		// Legacy keys boot in the provisioner-default namespace.
		ns := key.Namespace
		if ns == "" {
			ns = "default"
		}
		if err2 := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pod); err2 != nil {
			t.Fatalf("get pod %s: %v / %v", name, err, err2)
		}
	}
	now := time.Now()
	pod.CreationTimestamp = metav1.Time{Time: now.Add(-time.Duration(ageMinutes) * time.Minute)}
	// Metadata (creation instant) via the plain Update; the Ready
	// condition via the status subresource — the fake client keeps the
	// two halves separate exactly like the API server does.
	if err := c.Update(ctx, pod); err != nil {
		t.Fatalf("stamp %s creation: %v", name, err)
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:               corev1.PodReady,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: now.Add(-time.Duration(ageMinutes-1) * time.Minute)},
		}}
	}
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatalf("stamp %s ready=%v: %v", name, ready, err)
	}
}

// seedStalePod stamps the PRE-CHANGE orphan shape: the legacy two
// annotations only, no full-key set — unprovable, therefore reaped.
func seedStalePod(t *testing.T, ctx context.Context, c client.Client, name string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"app": "k8squad-sandbox", "sandbox": name},
			Annotations: map[string]string{
				"k8squad.io/sandbox-id": name,
				"k8squad.io/pool-key":   "gvisor/runtime:v1",
			},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Hour)},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("seed stale pod: %v", err)
	}
}

// seedRunOwnedSecret writes the Bind-path task-io Secret that marks a pod
// as a live Run's sandbox (SecretCredentialWriter's contract: created at
// Bind, before the durable marker).
func seedRunOwnedSecret(t *testing.T, ctx context.Context, c client.Client, ns, name string) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"token": []byte("run-scoped")},
	}
	if err := c.Create(ctx, secret); err != nil {
		t.Fatalf("seed run-owned secret: %v", err)
	}
}

// A1 + A3: the restart pass adopts provably-warm Ready pods, reaps
// pre-change orphans and unprovable/unmanaged pods, and leaves run-owned
// pods to the run-drive lifecycle.
func TestAdoptOrReapAdoptsWarmReapsStaleLeavesRunOwned(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)

	// The previous generation's warm pool: 5 Ready pods, staggered ages.
	for i := 0; i < 5; i++ {
		bootWarmPod(t, ctx, c, kp, gvisorKey, warmPodName(i), 10-i, true)
	}
	// A pre-change orphan (legacy annotations only).
	seedStalePod(t, ctx, c, "stale-orphan")
	// A live Run's sandbox: full annotations, Ready, but Bind claimed it.
	bootWarmPod(t, ctx, c, kp, gvisorKey, "run-owned", 3, true)
	seedRunOwnedSecret(t, ctx, c, "default", "run-owned")
	// A crashed generation's in-flight replenish (managed key, not Ready).
	bootWarmPod(t, ctx, c, kp, gvisorKey, "warming-leftover", 1, false)
	// Config drift: a pod for a key no controller manages.
	bootWarmPod(t, ctx, c, kp, kataKey, "unmanaged-key", 2, true)

	pool := warmpool.NewPool(newFakeProvisioner())
	report, err := warmpool.AdoptOrReap(ctx, c, pool, gvisorKey)
	if err != nil {
		t.Fatalf("adopt-or-reap: %v", err)
	}
	if report.Adopted != 5 {
		t.Errorf("adopted = %d, want 5 (the Ready, proven, unclaimed warm pods)", report.Adopted)
	}
	if report.LeftRunOwned != 1 {
		t.Errorf("leftRunOwned = %d, want 1 (the Secret-carrying pod)", report.LeftRunOwned)
	}
	// Stale orphan + warming leftover + unmanaged key = 3 reaped.
	if report.Reaped != 3 {
		t.Errorf("reaped = %d, want 3 (stale, non-Ready, unmanaged)", report.Reaped)
	}

	if got := pool.Inventory()[gvisorKey]; got.Ready != 5 {
		t.Errorf("post-adoption Ready inventory = %d, want 5", got.Ready)
	}

	// Dispositions on the cluster: the 5 warm pods and the run-owned pod
	// live; the other three are gone.
	for i := 0; i < 5; i++ {
		assertPodExists(t, ctx, c, "default", warmPodName(i))
	}
	assertPodExists(t, ctx, c, "default", "run-owned")
	for _, gone := range []string{"stale-orphan", "warming-leftover", "unmanaged-key"} {
		assertPodGone(t, ctx, c, "default", gone)
	}
}

// A2 — the ticket's core verification: after adoption, the controller's
// first Tick boots NOTHING (the adopted pods count toward live) and a
// warm Bind pops an adopted pod with zero provisioner calls.
func TestAdoptOrReapAdoptedPoolNeedsNoReplacementBoot(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	for i := 0; i < 5; i++ {
		bootWarmPod(t, ctx, c, kp, gvisorKey, warmPodName(i), 10-i, true)
	}

	fp := newFakeProvisioner()
	pool := warmpool.NewPool(fp)
	report, err := warmpool.AdoptOrReap(ctx, c, pool, gvisorKey)
	if err != nil {
		t.Fatalf("adopt-or-reap: %v", err)
	}
	if report.Adopted != 5 {
		t.Fatalf("adopted = %d, want 5 (sanity)", report.Adopted)
	}

	// The controller shape from cmd/operator: target pinned to 5
	// (KSQUAD_WARM_POOL_TARGET=5), λ=0.
	policy := newPolicy()
	policy.MinReady = 5
	warmController := warmpool.NewController(pool,
		warmpool.NewAutoscaler(policy, warmpool.DefaultStabilizationTicks),
		warmpool.ManagedKey{Key: gvisorKey, Class: warmpool.ClassInteractive, Pressure: warmpool.StaticPressure(0)})

	if _, err := warmController.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := fp.bootCount(); got != 0 {
		t.Fatalf("first tick after restart booted %d replacement pods, want 0 (the orphan-churn twin)", got)
	}
	if got := pool.Inventory()[gvisorKey]; got.Ready != 5 {
		t.Fatalf("ready = %d after tick, want 5 (nothing booted, nothing drained)", got.Ready)
	}

	// A claim warm-binds an ADOPTED pod: zero Boot calls, and the OLDEST
	// (FIFO) — warmPodName(0) carries the largest age (10 min) of the
	// five, so it sits at the Ready queue's front.
	ref, err := pool.Bind(ctx, "run-after-restart", gvisorKey, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if ref != warmPodName(0) {
		t.Errorf("warm bind popped %q, want the oldest adopted pod %q (FIFO)", ref, warmPodName(0))
	}
	if got := fp.bootCount(); got != 0 {
		t.Errorf("warm bind on adopted pool booted %d pods, want 0", got)
	}
}

// A4: Pool.Adopt is idempotent, forces the Ready/unbound posture, and the
// adopted id is the sandbox_ref a warm Bind hands out.
func TestPoolAdoptIdempotentAndClaimable(t *testing.T) {
	ctx := context.Background()
	fp := newFakeProvisioner()
	pool := warmpool.NewPool(fp)

	sb := warmpool.Sandbox{
		ID:        "adopted-1",
		Key:       gvisorKey,
		State:     warmpool.StateReady,
		CreatedAt: time.Now().Add(-time.Minute),
		ReadyAt:   time.Now(),
	}
	if !pool.Adopt(sb) {
		t.Fatal("first Adopt returned false, want true")
	}
	if pool.Adopt(sb) {
		t.Fatal("second Adopt returned true, want false (idempotent)")
	}

	inv := pool.Inventory()[gvisorKey]
	if inv.Ready != 1 || inv.Bound != 0 || inv.Warming != 0 {
		t.Fatalf("post-adopt inventory = %+v, want exactly 1 Ready", inv)
	}

	ref, err := pool.Bind(ctx, "run-1", gvisorKey, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if ref != "adopted-1" {
		t.Fatalf("bind ref = %q, want the adopted id", ref)
	}
	if got := fp.bootCount(); got != 0 {
		t.Fatalf("warm bind over adopted warmth booted %d, want 0", got)
	}
	// Teardown-and-replace flows through the normal Release path for
	// adopted warmth too.
	if err := pool.Release(ctx, "run-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := fp.teardownCount("adopted-1"); got != 1 {
		t.Fatalf("teardowns of adopted pod = %d, want 1", got)
	}
}

// Boot must stamp the FULL key — one annotation per dimension, empty
// values included: presence is the provability marker (a pod whose key
// cannot be proven is reaped, never adopted).
func TestKubeProvisionerBootStampsFullPoolKeyAnnotations(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	key := warmpool.PoolKey{
		RuntimeClass:   "gvisor",
		Image:          "reg.example/ksquad-shim-codex:m1",
		Namespace:      "squad-a",
		CapabilityHash: "9f2a",
		ProjectPVC:     "",
	}
	if err := kp.Boot(ctx, key, "sbx-fullkey", warmpool.BootWarm); err != nil {
		t.Fatalf("boot: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: "sbx-fullkey"}, pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	want := map[string]string{
		warmpool.AnnPoolRuntimeClass:   "gvisor",
		warmpool.AnnPoolImage:          "reg.example/ksquad-shim-codex:m1",
		warmpool.AnnPoolNamespace:      "squad-a",
		warmpool.AnnPoolCapabilityHash: "9f2a",
		warmpool.AnnPoolProjectPVC:     "", // empty but PRESENT
	}
	for ann, v := range want {
		got, ok := pod.Annotations[ann]
		if !ok {
			t.Errorf("annotation %s missing (empty dimensions must be stamped present, not omitted)", ann)
			continue
		}
		if got != v {
			t.Errorf("annotation %s = %q, want %q", ann, got, v)
		}
	}
	// The legacy display annotation stays for backward compatibility.
	if got := pod.Annotations["k8squad.io/pool-key"]; got != "gvisor/reg.example/ksquad-shim-codex:m1" {
		t.Errorf("legacy pool-key annotation = %q", got)
	}
}

// Boot must stamp the FULL key — one annotation per dimension, empty

func warmPodName(i int) string { return "warm-prev-gen-" + string(rune('a'+i)) }

func assertPodExists(t *testing.T, ctx context.Context, c client.Client, ns, name string) {
	t.Helper()
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Pod{}); err != nil {
		t.Errorf("pod %s/%s should still exist: %v", ns, name, err)
	}
}

func assertPodGone(t *testing.T, ctx context.Context, c client.Client, ns, name string) {
	t.Helper()
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Pod{})
	if err == nil || !apierrors.IsNotFound(err) {
		t.Errorf("pod %s/%s should be reaped, got err=%v", ns, name, err)
	}
}
