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

// Coverage-restore tests (ISI-6745): pure unit coverage for the error/branch
// paths the existing suite leaves cold — no production-code changes. Targets
// the 0%/low-coverage surfaces the ISI-6539 baseline refresh (PR #948)
// re-pinned pkg/warmpool DOWN to 79.4: ManifestForRun, WithPodEnv, the
// KubeCapacity* capacity reader, SetSettledReaper's Tick backstop, RecreateMeshPool,
// and the Acklam inverse-CDF tails in the sizing math.

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentmesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/warmpool"
)

// coverageScheme wires corev1 + the Run CRD group so the fake client can list
// both pods/nodes and Runs.
func coverageScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("api scheme: %v", err)
	}
	return s
}

// errReader is a client.Reader whose List always fails — the capacity/manifest
// readers must surface that error wrapped, not swallow it.
type errReader struct{ client.Reader }

func (errReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("boom: apiserver unreachable")
}

// errReaderClient is a full client.Client whose List fails — CRLister takes a
// client.Client, not a bare Reader, and must surface the list error.
type errReaderClient struct{ client.Client }

func (errReaderClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("boom: apiserver unreachable")
}

// TestManifestForRun covers the WithToolchainResolver adapter: the resolved
// toolchains come back for a Run whose status carries a manifest, and each
// fail-closed branch (no manifest, run absent, list error) returns an error.
func TestManifestForRun(t *testing.T) {
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "r-ok", Namespace: "squad-a", UID: types.UID("uid-ok")},
		Status: api.RunStatus{
			CapabilityManifest: &api.CapabilityManifest{
				Toolchains: []api.ResolvedToolchainRef{{
					Name: "kubectl", Version: "1.31", Image: "reg/kubectl@sha256:abc",
					SourceNamespace: "ksquad-system", Provides: []string{"kubectl"},
				}},
			},
		},
	}
	noMan := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "r-bare", Namespace: "squad-a", UID: types.UID("uid-bare")},
	}
	c := fake.NewClientBuilder().WithScheme(coverageScheme(t)).WithObjects(run, noMan).Build()
	resolve := warmpool.ManifestForRun(c)
	ctx := context.Background()

	// Happy path: the recorded toolchain envelope is rebuilt verbatim.
	got, err := resolve(ctx, "uid-ok")
	if err != nil {
		t.Fatalf("resolve(uid-ok): %v", err)
	}
	if len(got) != 1 || got[0].Name != "kubectl" || got[0].Image != "reg/kubectl@sha256:abc" {
		t.Fatalf("resolved toolchains = %+v, want single kubectl entry", got)
	}

	// A Run with no manifest fails CLOSED (the ISI-5221 silent tools-absent boot).
	if _, err := resolve(ctx, "uid-bare"); err == nil {
		t.Fatal("run without a capability manifest must fail closed")
	}

	// A Run that cannot be found fails closed.
	if _, err := resolve(ctx, "uid-ghost"); err == nil {
		t.Fatal("missing run must fail closed")
	}

	// A list failure is surfaced, not swallowed.
	if _, err := warmpool.ManifestForRun(errReader{})(ctx, "uid-ok"); err == nil {
		t.Fatal("list error must propagate")
	}
}

// TestWithPodEnv pins that the OTLP-passthrough env the provisioner is told to
// stamp actually lands in every sandbox container, and that the setter chains.
func TestWithPodEnv(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(coverageScheme(t)).Build()
	p := warmpool.NewKubeProvisioner(c, "1", "512Mi")
	if p.WithPodEnv(corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://collector:4318"}) != p {
		t.Fatal("WithPodEnv must return the receiver for chaining")
	}
	key := warmpool.PoolKey{RuntimeClass: "gvisor", Namespace: "squad-a", Image: "reg/shim:test"}
	if err := p.Boot(context.Background(), key, "sbx-env", "", warmpool.BootWarm); err != nil {
		t.Fatalf("boot: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "squad-a", Name: "sbx-env"}, pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	var found string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "OTEL_EXPORTER_OTLP_ENDPOINT" {
			found = e.Value
		}
	}
	if found != "http://collector:4318" {
		t.Fatalf("stamped OTLP endpoint = %q, want the passthrough value", found)
	}
}

// TestKubeCapacitySnapshot covers the live cluster-capacity reader: a Ready
// schedulable node contributes its allocatable CPU; a NotReady node and a
// cordoned node contribute nothing; running pod CPU requests are subtracted
// while terminal (Succeeded/Failed) pods are ignored. KubeCapacitySource is
// the controller adapter over the same read, and the list path fails loudly.
func TestKubeCapacitySnapshot(t *testing.T) {
	node := func(name string, milliCPU int64, ready bool, cordoned bool) *corev1.Node {
		st := corev1.ConditionFalse
		if ready {
			st = corev1.ConditionTrue
		}
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.NodeSpec{Unschedulable: cordoned},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(milliCPU, resource.DecimalSI)},
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}},
			},
		}
	}
	pod := func(name string, phase corev1.PodPhase, reqMilli int64) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:      "c",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(reqMilli, resource.DecimalSI)}},
			}}},
			Status: corev1.PodStatus{Phase: phase},
		}
	}
	c := fake.NewClientBuilder().WithScheme(coverageScheme(t)).WithObjects(
		node("ready", 4000, true, false),      // +4000 alloc
		node("notready", 8000, false, false),  // skipped — dead
		node("cordoned", 8000, true, true),    // skipped — cordoned
		pod("live", corev1.PodRunning, 1500),  // -1500 used
		pod("done", corev1.PodSucceeded, 999), // skipped — terminal
		pod("dead", corev1.PodFailed, 999),    // skipped — terminal
	).Build()

	alloc, used, free, err := warmpool.KubeCapacitySnapshot(context.Background(), c)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if alloc != 4000 || used != 1500 || free != 2500 {
		t.Fatalf("snapshot alloc/used/free = %d/%d/%d, want 4000/1500/2500", alloc, used, free)
	}

	src := warmpool.KubeCapacitySource(c)
	gotFree, err := src(context.Background())
	if err != nil {
		t.Fatalf("capacity source: %v", err)
	}
	if gotFree != 2500 {
		t.Fatalf("capacity source free = %d, want 2500", gotFree)
	}

	// A failed node/pod list fails loudly through both the snapshot and the source.
	if _, _, _, err := warmpool.KubeCapacitySnapshot(context.Background(), errReader{}); err == nil {
		t.Fatal("snapshot must surface a list error")
	}
	if _, err := warmpool.KubeCapacitySource(errReader{})(context.Background()); err == nil {
		t.Fatal("capacity source must surface a list error")
	}
}

// TestRecreateMeshPool covers the gateway-down repair path (ISI-6010): the
// Team/Role guard fails closed, and the happy path on an absent pod deletes
// (NotFound is fine), drains, and re-boots a standing pod from spec.
func TestRecreateMeshPool(t *testing.T) {
	k, cl := newMeshProvisioner(t)
	ctx := context.Background()

	if err := k.RecreateMeshPool(ctx, warmpool.MeshPoolSpec{Role: "coder"}); err == nil {
		t.Fatal("RecreateMeshPool with empty Team must fail closed")
	}
	if err := k.RecreateMeshPool(ctx, warmpool.MeshPoolSpec{Team: "t"}); err == nil {
		t.Fatal("RecreateMeshPool with empty Role must fail closed")
	}

	spec := warmpool.MeshPoolSpec{Team: "team-acme", Role: "coder", Image: "img"}
	if err := k.RecreateMeshPool(ctx, spec); err != nil {
		t.Fatalf("recreate from absent pod: %v", err)
	}
	pod := &corev1.Pod{}
	name := warmpool.MeshPoolPodName(spec.Team, spec.Role)
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, pod); err != nil {
		t.Fatalf("recreate did not leave a standing pod: %v", err)
	}
}

// TestSetSettledReaperRunsInTick covers the SetSettledReaper setter and the
// Tick backstop branch that invokes it (ADR-0020 §2.3): once installed the
// closure runs at the top of every Tick; nil disables it without panicking.
func TestSetSettledReaperRunsInTick(t *testing.T) {
	c, pool, fp, _ := newController(t, warmpool.ClassInteractive)
	calls := 0
	c.SetSettledReaper(func(context.Context) { calls++ })
	tickReady(t, c, pool, fp)
	if calls != 1 {
		t.Fatalf("settled reaper ran %d times, want exactly 1 per tick", calls)
	}
	// nil disables the backstop — the next tick must not panic on a nil call.
	c.SetSettledReaper(nil)
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatalf("tick after disabling reaper: %v", err)
	}
	if calls != 1 {
		t.Fatalf("settled reaper ran again after being disabled: %d", calls)
	}
}

// TestRecommendBufferAcklamTails covers zFromServiceLevel's two rational-
// approximation tails — the lower tail (sl < 0.02425) and the upper tail
// (sl > 0.97575), reached only for extreme non-tabulated service levels.
// Monotonicity in the service level pins the sign of each tail.
func TestRecommendBufferAcklamTails(t *testing.T) {
	lowTail, err := warmpool.RecommendBuffer(0.2, 4.0, 0.01) // sl < plow
	if err != nil {
		t.Fatalf("low-tail buffer: %v", err)
	}
	mid, err := warmpool.RecommendBuffer(0.2, 4.0, 0.95)
	if err != nil {
		t.Fatalf("mid buffer: %v", err)
	}
	highTail, err := warmpool.RecommendBuffer(0.2, 4.0, 0.98) // sl > phigh
	if err != nil {
		t.Fatalf("high-tail buffer: %v", err)
	}
	// A tiny service level wants a NEGATIVE z (under-provision toward the
	// cycle-demand floor); a near-certain one wants a larger safety stock.
	if lowTail > mid || mid > highTail {
		t.Fatalf("buffer not monotonic across Acklam tails: low=%d mid=%d high=%d", lowTail, mid, highTail)
	}
	if lowTail < 1 {
		t.Fatalf("buffer floor violated at low tail: %d", lowTail)
	}
}

// TestCRListerListRolePools covers the production RolePool→envelope resolver:
// a fully-resolvable pool lands as a RolePoolConfig; a pool whose roleRef or
// runtimeRef dangles is reported in the unresolved map (never dropped, never
// a scheduler input); and a list failure propagates.
func TestCRListerListRolePools(t *testing.T) {
	role := &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-acme"},
		Spec: api.RoleSpec{
			Model:         "gpt-5.2-codex",
			Coordinator:   true,
			DefaultSkills: []api.ObjectRef{{Name: "go"}, {Name: "git"}},
		},
	}
	rt := &api.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "codex-rt", Namespace: "team-acme"},
		Spec:       api.AgentRuntimeSpec{Type: "codex"},
	}
	good := &api.RolePool{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-pool", Namespace: "team-acme", Generation: 7},
		Spec: api.RolePoolSpec{
			RoleRef:    api.ObjectRef{Name: "coder"},
			RuntimeRef: api.ObjectRef{Name: "codex-rt"},
			Standing:   api.RolePoolStanding{Min: 2, Max: 5},
			Lifecycle:  api.RolePoolLifecycle{CheckInterval: "45s", AlwaysUp: true},
		},
	}
	badRole := &api.RolePool{
		ObjectMeta: metav1.ObjectMeta{Name: "ghost-role-pool", Namespace: "team-acme"},
		Spec: api.RolePoolSpec{
			RoleRef:    api.ObjectRef{Name: "missing-role"},
			RuntimeRef: api.ObjectRef{Name: "codex-rt"},
			Standing:   api.RolePoolStanding{Min: 1, Max: 1},
		},
	}
	badRuntime := &api.RolePool{
		ObjectMeta: metav1.ObjectMeta{Name: "ghost-runtime-pool", Namespace: "team-acme"},
		Spec: api.RolePoolSpec{
			RoleRef:    api.ObjectRef{Name: "coder"},
			RuntimeRef: api.ObjectRef{Name: "missing-runtime"},
			Standing:   api.RolePoolStanding{Min: 1, Max: 1},
		},
	}
	c := fake.NewClientBuilder().WithScheme(coverageScheme(t)).
		WithObjects(role, rt, good, badRole, badRuntime).Build()
	lister := &warmpool.CRLister{Client: c}

	pools, unresolved, err := lister.ListRolePools(context.Background())
	if err != nil {
		t.Fatalf("ListRolePools: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("resolved pools = %d, want exactly the one fully-referenced pool", len(pools))
	}
	got := pools[0]
	if got.Name != "coder-pool" || got.Role != "coder" || got.RuntimeType != "codex" {
		t.Fatalf("resolved config = %+v, want coder-pool/coder/codex", got)
	}
	if got.Min != 2 || got.Max != 5 || !got.Coordinator || got.Model != "gpt-5.2-codex" {
		t.Fatalf("resolved config band/role facts wrong: %+v", got)
	}
	if len(got.Skills) != 2 || got.Skills[0] != "go" || got.Generation != 7 {
		t.Fatalf("resolved skills/generation wrong: %+v", got)
	}
	if _, ok := unresolved["team-acme/ghost-role-pool"]; !ok {
		t.Fatal("dangling roleRef must be reported unresolved")
	}
	if _, ok := unresolved["team-acme/ghost-runtime-pool"]; !ok {
		t.Fatal("dangling runtimeRef must be reported unresolved")
	}

	if _, _, err := (&warmpool.CRLister{Client: errReaderClient{}}).ListRolePools(context.Background()); err == nil {
		t.Fatal("ListRolePools must surface a list error")
	}
}

// TestClientStatusWriterWritePoolStatus covers the CRD status-subresource
// writer: it patches the fields it owns for an existing pool and fails loudly
// when the pool is absent.
func TestClientStatusWriterWritePoolStatus(t *testing.T) {
	rp := &api.RolePool{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-pool", Namespace: "team-acme"},
		Spec: api.RolePoolSpec{
			RoleRef:    api.ObjectRef{Name: "coder"},
			RuntimeRef: api.ObjectRef{Name: "codex-rt"},
			Standing:   api.RolePoolStanding{Min: 2, Max: 5},
		},
	}
	c := fake.NewClientBuilder().WithScheme(coverageScheme(t)).
		WithObjects(rp).WithStatusSubresource(&api.RolePool{}).Build()
	w := &warmpool.ClientStatusWriter{Client: c}

	plan := agentmesh.PoolPlan{Team: "team-acme", Name: "coder-pool", Role: "coder"}
	report := agentmesh.PoolStatusReport{Target: 3, Standing: 2, StandingKnown: true, ObservedGeneration: 7}
	if err := w.WritePoolStatus(context.Background(), plan, report); err != nil {
		t.Fatalf("WritePoolStatus: %v", err)
	}
	got := &api.RolePool{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "team-acme", Name: "coder-pool"}, got); err != nil {
		t.Fatalf("get after status write: %v", err)
	}
	if got.Status.Target != 3 || got.Status.Standing != 2 || got.Status.ObservedGeneration != 7 {
		t.Fatalf("written status = %+v, want target=3 standing=2 gen=7", got.Status)
	}
	if got.Status.Pod != warmpool.MeshPoolPodName("team-acme", "coder") {
		t.Fatalf("written pod name = %q, want the mesh pool pod name", got.Status.Pod)
	}

	// An absent pool fails loudly.
	absent := agentmesh.PoolPlan{Team: "team-acme", Name: "ghost", Role: "coder"}
	if err := w.WritePoolStatus(context.Background(), absent, report); err == nil {
		t.Fatal("WritePoolStatus on a missing pool must fail loudly")
	}
}
