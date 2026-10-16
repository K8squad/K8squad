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

// rolepool_test.go — the P1.3 POLICY #1 pins (chapter §4 D2.2, ISI-5824;
// P3.5 leak L3, ISI-6038): the 12/5→2–3 math resolved through the
// SchedulingPolicy seam, the coordinator always-up floor, the D9.3
// flagged-team blast radius, the unresolved-reference skip — and the
// acceptance proof that RolePool drives the mesh Scheduler through the
// policy interface ONLY (module agentmesh.NewScheduler + this policy, no
// host loop). The selection/saturation halves of the proof live in
// pkg/mesh/gateway_band_test.go; the generic loop's own pins live in the
// module (mesh/scheduling_test.go).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentmesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/K8squad/K8squad/pkg/warmpool"
)

// rolePoolLister serves a fixed resolved set (or a listing failure).
type rolePoolLister struct {
	pools      []warmpool.RolePoolConfig
	unresolved map[string]error
	err        error
}

func (f *rolePoolLister) ListRolePools(context.Context) ([]warmpool.RolePoolConfig, map[string]error, error) {
	return f.pools, f.unresolved, f.err
}

// meshPoolProvisioner records the plans the mesh Scheduler asked to stand up.
type meshPoolProvisioner struct {
	mu       sync.Mutex
	plans    []agentmesh.PoolPlan
	failTeam string
}

func (f *meshPoolProvisioner) EnsurePool(_ context.Context, plan agentmesh.PoolPlan) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failTeam != "" && plan.Team == f.failTeam {
		return errors.New("provisioner: injected failure")
	}
	f.plans = append(f.plans, plan)
	return nil
}

func (f *meshPoolProvisioner) snapshot() []agentmesh.PoolPlan {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]agentmesh.PoolPlan, len(f.plans))
	copy(out, f.plans)
	return out
}

// meshPoolCounter serves per-(team, role) standing counts; nil counts error.
type meshPoolCounter struct {
	mu     sync.Mutex
	counts map[string]int // "team/role"
}

func (f *meshPoolCounter) StandingProcesses(_ context.Context, team, role string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts == nil {
		return 0, errors.New("counter: no data")
	}
	n, ok := f.counts[team+"/"+role]
	if !ok {
		return 0, errors.New("counter: unknown pool")
	}
	return n, nil
}

// meshPoolStatus records the bounded reports written per pool.
type meshPoolStatus struct {
	mu      sync.Mutex
	written map[string]agentmesh.PoolStatusReport
}

func (f *meshPoolStatus) WritePoolStatus(_ context.Context, plan agentmesh.PoolPlan, report agentmesh.PoolStatusReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.written == nil {
		f.written = map[string]agentmesh.PoolStatusReport{}
	}
	f.written[plan.Team+"/"+plan.Name] = report
	return nil
}

func (f *meshPoolStatus) get(key string) (agentmesh.PoolStatusReport, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.written[key]
	return r, ok
}

// roster is the issue's proving roster: FIVE roles at min 2–3 each —
// the "12 agents / 5 roles → 2–3 per pool" shape of §9.1/§2.3, with the
// coordinator always-up (floor ≥ 1, sweep-exempt).
func roster(team string) []warmpool.RolePoolConfig {
	mk := func(name string, min, max int, coordinator bool) warmpool.RolePoolConfig {
		return warmpool.RolePoolConfig{
			Namespace:   team,
			Name:        name,
			Role:        name,
			RuntimeType: "opencode",
			Min:         min,
			Max:         max,
			Coordinator: coordinator,
			Model:       "glm-4.7",
			Generation:  1,
		}
	}
	return []warmpool.RolePoolConfig{
		mk("coordinator", 2, 2, true),
		mk("coder", 3, 3, false),
		mk("reviewer", 2, 3, false),
		mk("implementer", 2, 3, false),
		mk("tester", 3, 3, false),
	}
}

func newRolePoolPolicy(lister warmpool.RolePoolLister, teams ...string) *warmpool.RolePoolPolicy {
	return warmpool.NewRolePoolPolicy(lister, teams, nil)
}

// THE ACCEPTANCE PIN: RolePool drives the mesh through the policy
// interface only — the module's generic Scheduler (agentmesh.NewScheduler)
// runs the loop; this policy is the only RolePool knowledge in the path.
// Five flagged RolePools reconcile into five ensured pools, the floors
// summing to exactly 12 standing processes across the team, every pool
// getting its bounded status with the target and the counter's standing
// count.
func TestRolePoolDrivesMeshScheduler_12Over5Math(t *testing.T) {
	policy := newRolePoolPolicy(&rolePoolLister{pools: roster("team-acme")}, "team-acme")
	provisioner := &meshPoolProvisioner{}
	status := &meshPoolStatus{}
	counter := &meshPoolCounter{counts: map[string]int{}}
	sched := agentmesh.NewScheduler(policy, provisioner, counter, status)

	for _, rp := range roster("team-acme") {
		counter.counts["team-acme/"+rp.Role] = rp.Min // registry mirror reports the floors standing
	}
	errs, rerr := sched.Tick(context.Background())
	if rerr != nil {
		t.Fatalf("resolve error: %v", rerr)
	}
	if len(errs) != 0 {
		t.Fatalf("reconcile errors: %v", errs)
	}

	got := provisioner.snapshot()
	if len(got) != 5 {
		t.Fatalf("ensured %d pools, want 5 (one per role)", len(got))
	}
	sum := 0
	for _, plan := range got {
		if plan.Team != "team-acme" || plan.Role == "" {
			t.Fatalf("bad pool identity: %+v", plan)
		}
		if plan.Min < 2 || plan.Min > 3 {
			t.Fatalf("pool %s min=%d, want the 2–3 per-pool band", plan.Role, plan.Min)
		}
		if plan.Max < plan.Min {
			t.Fatalf("pool %s max=%d < min=%d", plan.Role, plan.Max, plan.Min)
		}
		if plan.Envelope.RuntimeType != "opencode" || plan.Envelope.Model != "glm-4.7" {
			t.Fatalf("pool %s missing resolved envelope: %+v", plan.Role, plan.Envelope)
		}
		sum += plan.Min
	}
	if sum != 12 {
		t.Fatalf("standing floors sum to %d across 5 roles, want 12", sum)
	}

	for _, rp := range roster("team-acme") {
		report, ok := status.get("team-acme/" + rp.Name)
		if !ok {
			t.Fatalf("no status written for %s", rp.Name)
		}
		if report.Target != int32(rp.Min) {
			t.Fatalf("status target = %d, want %d", report.Target, rp.Min)
		}
		if !report.StandingKnown || report.Standing != int32(rp.Min) {
			t.Fatalf("status standing = %d (known=%v), want the counter's %d", report.Standing, report.StandingKnown, rp.Min)
		}
	}
}

// The coordinator floor (D2.2), resolved by the POLICY: a coordinator
// RolePool (or alwaysUp) never targets below one standing process and its
// plan carries AlwaysUp so the gateway's sweep exempts it; a plain min:0
// pool keeps target 0 — pure on-demand.
func TestRolePoolPolicy_CoordinatorFloor(t *testing.T) {
	policy := newRolePoolPolicy(&rolePoolLister{pools: []warmpool.RolePoolConfig{{
		Namespace: "team-acme", Name: "coordinator", Role: "coordinator",
		Min: 0, Max: 1, // floor zero — the coordinator rule must lift it to 1
		Coordinator: true, Generation: 7,
	}}}, "team-acme")

	plans, err := policy.ResolvePools(context.Background())
	if err != nil {
		t.Fatalf("ResolvePools: %v", err)
	}
	if len(plans) != 1 || plans[0].Min != 1 {
		t.Fatalf("coordinator plan = %+v, want min lifted to the floor 1", plans)
	}
	if plans[0].Lifecycle == nil || !plans[0].Lifecycle.AlwaysUp {
		t.Fatalf("coordinator plan must carry AlwaysUp: %+v", plans[0].Lifecycle)
	}

	plain := newRolePoolPolicy(&rolePoolLister{pools: []warmpool.RolePoolConfig{{
		Namespace: "team-acme", Name: "ondemand", Role: "ondemand", Min: 0, Max: 2,
	}}}, "team-acme")
	plans, err = plain.ResolvePools(context.Background())
	if err != nil {
		t.Fatalf("ResolvePools: %v", err)
	}
	if len(plans) != 1 || plans[0].Min != 0 {
		t.Fatalf("plain pool plan = %+v, want target 0 (on-demand)", plans)
	}
	if plans[0].Lifecycle == nil || plans[0].Lifecycle.AlwaysUp {
		t.Fatalf("non-coordinator plan must not be always-up: %+v", plans[0].Lifecycle)
	}
}

// Lifecycle knobs ride the plan in their generic (parsed) shape: valid
// duration strings parse, malformed ones degrade to zero — the gateway
// default cadence, never a wrong-shaped pod.
func TestRolePoolPolicy_LifecycleKnobsCarried(t *testing.T) {
	policy := newRolePoolPolicy(&rolePoolLister{pools: []warmpool.RolePoolConfig{{
		Namespace: "team-acme", Name: "coder", Role: "coder", Min: 1, Max: 2,
		Lifecycle: warmpool.MeshLifecycleSpec{
			CheckInterval:   "45s",
			IdleToHibernate: "10m",
			HibernateToStop: "not-a-duration", // degrades to zero, not an error
		},
	}}}, "team-acme")

	plans, err := policy.ResolvePools(context.Background())
	if err != nil {
		t.Fatalf("ResolvePools: %v", err)
	}
	lc := plans[0].Lifecycle
	if lc == nil {
		t.Fatalf("plan carries no lifecycle: %+v", plans[0])
	}
	if lc.CheckInterval != 45*time.Second || lc.IdleToHibernate != 10*time.Minute {
		t.Fatalf("lifecycle knobs not carried: %+v", lc)
	}
	if lc.HibernateToStop != 0 {
		t.Fatalf("malformed knob must degrade to zero, got %v", lc.HibernateToStop)
	}
}

// D9.3 blast radius: a RolePool in an UNFLAGGED team is skipped entirely —
// the canary flag set is the rollout unit, and a stray CR in another
// namespace plans nothing.
func TestRolePoolPolicy_UnflaggedTeamSkipped(t *testing.T) {
	policy := newRolePoolPolicy(&rolePoolLister{pools: roster("team-other")}, "team-canary")

	plans, err := policy.ResolvePools(context.Background())
	if err != nil {
		t.Fatalf("ResolvePools: %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("planned %d pools for an unflagged team, want 0", len(plans))
	}
}

// A half-configured CR never reaches the scheduler: unresolved references
// are skipped and reported through the onUnresolved hook; the resolvable
// pools still plan.
func TestRolePoolPolicy_UnresolvedRefsSkippedAndReported(t *testing.T) {
	lister := &rolePoolLister{
		pools: roster("team-acme")[:1],
		unresolved: map[string]error{
			"team-acme/broken": errors.New("resolve roleRef \"nope\": not found"),
		},
	}
	var reported []string
	policy := warmpool.NewRolePoolPolicy(lister, []string{"team-acme"}, func(key string, err error) {
		if err == nil {
			t.Fatalf("unresolved hook called without an error for %s", key)
		}
		reported = append(reported, key)
	})

	plans, err := policy.ResolvePools(context.Background())
	if err != nil {
		t.Fatalf("ResolvePools: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("planned %d pools, want the 1 resolvable one", len(plans))
	}
	if len(reported) != 1 || reported[0] != "team-acme/broken" {
		t.Fatalf("unresolved reported = %v, want [team-acme/broken]", reported)
	}
}

// A listing failure fails the whole pass: the policy's source is unreadable,
// so nothing can be trusted this tick.
func TestRolePoolPolicy_ResolveErrorPropagates(t *testing.T) {
	policy := newRolePoolPolicy(&rolePoolLister{err: errors.New("api server down")}, "team-acme")
	if _, err := policy.ResolvePools(context.Background()); err == nil {
		t.Fatalf("listing failure must propagate")
	}
}

// THE COMPOSITION-ROOT PIN (P2 close-out follow-up, ISI-5826 b09e1970):
// KubeEnsurer maps one generic plan onto the standing pool pod — the
// resolved runtime type stamped as KSQUAD_RUNTIME_TYPE (the in-pod
// composition root binds the matching RuntimeAdapter from it), the image
// resolved from the runtime type, the band and envelope from the plan,
// the substrate placement (sandbox namespace, ingress, PVC) from the host
// wiring. An untyped plan boots no runtime env — byte-identical to the
// pre-seam pod shape.
func TestKubeEnsurer_EnsurePoolBootsTypedPod(t *testing.T) {
	k, cl := newMeshProvisioner(t)
	ensurer := &warmpool.KubeEnsurer{
		Provisioner: k,
		ImageFor: func(runtimeType string) string {
			return "ghcr.io/k8squad/shim-" + runtimeType + ":dev"
		},
		PodNS:   "ksquad-sandbox",
		Ingress: "http://operator:8090/mesh",
		WorkPVC: "proj-workspace",
	}

	err := ensurer.EnsurePool(context.Background(), agentmesh.PoolPlan{
		Team: "team-acme", Name: "coder", Role: "coder",
		Envelope: agentmesh.Envelope{RuntimeType: "codex", Model: "gpt-5.2-codex", Skills: []string{"go"}},
		Min:      2, Max: 3,
		Lifecycle: &agentmesh.LifecycleConfig{
			CheckInterval:   45 * time.Second,
			IdleToHibernate: 10 * time.Minute,
			AlwaysUp:        true,
		},
		Generation: 4,
	})
	if err != nil {
		t.Fatalf("EnsurePool: %v", err)
	}

	var pod corev1.Pod
	if err := cl.Get(context.Background(), client.ObjectKey{
		Namespace: "ksquad-sandbox", Name: warmpool.MeshPoolPodName("team-acme", "coder"),
	}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"KSQUAD_RUNTIME_TYPE":                     "codex",
		"KSQUAD_MESH_MODEL":                       "gpt-5.2-codex",
		"KSQUAD_MESH_SKILLS":                      "go",
		"KSQUAD_MESH_MIN":                         "2",
		"KSQUAD_MESH_MAX":                         "3",
		"KSQUAD_MESH_LIFECYCLE_CHECK_INTERVAL":    "45s",
		"KSQUAD_MESH_LIFECYCLE_IDLE_TO_HIBERNATE": "10m0s",
		"KSQUAD_MESH_LIFECYCLE_ALWAYS_UP":         "true",
		"KSQUAD_SUPERVISOR_INGRESS_URL":           "http://operator:8090/mesh",
	} {
		if env[name] != want {
			t.Fatalf("env %s = %q, want %q", name, env[name], want)
		}
	}
	if pod.Spec.Containers[0].Image != "ghcr.io/k8squad/shim-codex:dev" {
		t.Fatalf("image = %q, want the runtime-resolved sandbox image", pod.Spec.Containers[0].Image)
	}

	// An untyped plan (the P1.1 imperative shape) stamps nothing new —
	// the pod the pre-seam operator booted, byte-identical.
	k2, cl2 := newMeshProvisioner(t)
	plain := &warmpool.KubeEnsurer{
		Provisioner: k2,
		ImageFor:    func(string) string { return "ghcr.io/k8squad/shim-opencode:dev" },
		PodNS:       "ksquad-sandbox",
	}
	if err := plain.EnsurePool(context.Background(), agentmesh.PoolPlan{
		Team: "team-acme", Name: "coder", Role: "coder", Min: 1, Max: 1,
		Envelope: agentmesh.Envelope{}, // untyped: the P1.1 imperative shape
	}); err != nil {
		t.Fatalf("EnsurePool (untyped): %v", err)
	}
	var bare corev1.Pod
	if err := cl2.Get(context.Background(), client.ObjectKey{
		Namespace: "ksquad-sandbox", Name: warmpool.MeshPoolPodName("team-acme", "coder"),
	}, &bare); err != nil {
		t.Fatalf("get bare pod: %v", err)
	}
	if v := envOf(t, &bare, "KSQUAD_RUNTIME_TYPE"); v != "" {
		t.Fatalf("untyped plan must not stamp KSQUAD_RUNTIME_TYPE, got %q", v)
	}
}

// The ensure failure rides the per-pool error map (the module loop pins
// the fan-out isolation; this proves the host adapter's error shape).
func TestKubeEnsurer_UnknownRuntimeTypeFailsBoot(t *testing.T) {
	k, _ := newMeshProvisioner(t)
	ensurer := &warmpool.KubeEnsurer{
		Provisioner: k,
		ImageFor:    func(string) string { return "" }, // unknown type → no image
		PodNS:       "ksquad-sandbox",
	}
	err := ensurer.EnsurePool(context.Background(), agentmesh.PoolPlan{
		Team: "team-acme", Name: "alien", Role: "alien",
		Envelope: agentmesh.Envelope{RuntimeType: "martian"},
		Min:      1, Max: 1,
	})
	if err == nil {
		t.Fatalf("a pool with no resolvable image must fail closed, never boot a wrong-shaped pod")
	}
}
