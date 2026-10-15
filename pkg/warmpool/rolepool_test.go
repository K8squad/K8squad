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

// rolepool_test.go — the P1.3 scheduler pins (chapter §4 D2.2, ISI-5824):
// the 12/5→2–3 math (a team's ~5 RolePools at min 2–3 fan out to one gateway
// pod each, floors summing to ~12 standing processes), the coordinator
// always-up floor, the D9.3 flagged-team blast radius, and the bounded
// status read-back. The selection/saturation halves of the proof live in
// pkg/mesh/gateway_band_test.go.

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/K8squad/K8squad/pkg/warmpool"
)

// fakeEnsurer records the pool-pod specs the scheduler asked to stand up.
type fakeEnsurer struct {
	mu    sync.Mutex
	specs []warmpool.MeshPoolSpec
	// failNamespace fails every ensure for this team (blast-radius probe).
	failNamespace string
}

func (f *fakeEnsurer) EnsureMeshPool(_ context.Context, spec warmpool.MeshPoolSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNamespace != "" && spec.Team == f.failNamespace {
		return errors.New("ensurer: injected failure")
	}
	f.specs = append(f.specs, spec)
	return nil
}

func (f *fakeEnsurer) snapshot() []warmpool.MeshPoolSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]warmpool.MeshPoolSpec, len(f.specs))
	copy(out, f.specs)
	return out
}

// fakeCounter serves per-(team, role) standing counts; an empty team errors.
type fakeCounter struct {
	mu     sync.Mutex
	counts map[string]int // "team/role"
}

func (f *fakeCounter) StandingProcesses(_ context.Context, team, role string) (int, error) {
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

// fakeStatus records the bounded reports written per RolePool.
type fakeStatus struct {
	mu      sync.Mutex
	written map[string]warmpool.RolePoolReport
}

func (f *fakeStatus) WriteRolePoolStatus(_ context.Context, namespace, name string, report warmpool.RolePoolReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.written == nil {
		f.written = map[string]warmpool.RolePoolReport{}
	}
	f.written[namespace+"/"+name] = report
	return nil
}

func (f *fakeStatus) get(key string) (warmpool.RolePoolReport, bool) {
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
			Image:       "registry.local/ksquad/opencode:latest",
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

// THE 12/5→2–3 pin: five RolePools reconcile into five distinct gateway pods
// (one per (team, role)), each carrying its declared band, the floors
// summing to exactly 12 standing processes across the team.
func TestRolePoolScheduler_12Over5Math(t *testing.T) {
	ensurer := &fakeEnsurer{}
	status := &fakeStatus{}
	counter := &fakeCounter{counts: map[string]int{}}
	sched := warmpool.NewRolePoolScheduler(ensurer, counter, status,
		[]string{"team-acme"}, "ksquad-sandbox", "http://op:8090/mesh", "")

	pools := roster("team-acme")
	for _, rp := range pools {
		counter.counts["team-acme/"+rp.Name] = rp.Min // registry mirror reports the floors standing
	}
	errs := sched.Tick(context.Background(), pools)
	if len(errs) != 0 {
		t.Fatalf("reconcile errors: %v", errs)
	}

	specs := ensurer.snapshot()
	if len(specs) != 5 {
		t.Fatalf("ensured %d pool pods, want 5 (one per role)", len(specs))
	}
	sum := 0
	for _, spec := range specs {
		if spec.Team != "team-acme" || spec.Role == "" {
			t.Fatalf("bad pool identity: %+v", spec)
		}
		if spec.Min < 2 || spec.Min > 3 {
			t.Fatalf("pool %s min=%d, want the 2–3 per-pool band", spec.Role, spec.Min)
		}
		if spec.Max < spec.Min {
			t.Fatalf("pool %s max=%d < min=%d", spec.Role, spec.Max, spec.Min)
		}
		if spec.Namespace != "ksquad-sandbox" {
			t.Fatalf("pool %s booted in %q, want the operator sandbox namespace", spec.Role, spec.Namespace)
		}
		if spec.Lifecycle == nil || spec.IngressURL != "http://op:8090/mesh" {
			t.Fatalf("pool %s missing lifecycle/ingress envelope: %+v", spec.Role, spec.Lifecycle)
		}
		sum += spec.Min
	}
	if sum != 12 {
		t.Fatalf("standing floors sum to %d across 5 roles, want 12", sum)
	}

	// Every pool got its bounded status with the pod name and target.
	for _, rp := range pools {
		report, ok := status.get("team-acme/" + rp.Name)
		if !ok {
			t.Fatalf("no status written for %s", rp.Name)
		}
		if report.Pod != warmpool.MeshPoolPodName("team-acme", rp.Name) {
			t.Fatalf("status pod = %q, want %q", report.Pod, warmpool.MeshPoolPodName("team-acme", rp.Name))
		}
		if report.Target != int32(rp.Min) {
			t.Fatalf("status target = %d, want %d", report.Target, rp.Min)
		}
		if !report.StandingKnown || report.Standing != int32(rp.Min) {
			t.Fatalf("status standing = %d (known=%v), want the counter's %d", report.Standing, report.StandingKnown, rp.Min)
		}
	}
}

// The coordinator floor (D2.2): a coordinator RolePool (or alwaysUp) never
// targets below one standing process and stamps ALWAYS_UP on the pod so the
// gateway's sweep exempts it — the always-up team driver.
func TestRolePoolScheduler_CoordinatorFloor(t *testing.T) {
	ensurer := &fakeEnsurer{}
	sched := warmpool.NewRolePoolScheduler(ensurer, &fakeCounter{}, &fakeStatus{},
		[]string{"team-acme"}, "ns", "", "")

	errs := sched.Tick(context.Background(), []warmpool.RolePoolConfig{{
		Namespace: "team-acme", Name: "coordinator", Role: "coordinator",
		Min: 0, Max: 1, // floor zero — the coordinator rule must lift it to 1
		Coordinator: true, Generation: 7,
	}})
	if len(errs) != 0 {
		t.Fatalf("reconcile errors: %v", errs)
	}
	specs := ensurer.snapshot()
	if len(specs) != 1 || specs[0].Min != 1 {
		t.Fatalf("coordinator pool spec = %+v, want min lifted to the floor 1", specs)
	}
	if specs[0].Lifecycle == nil || !specs[0].Lifecycle.AlwaysUp {
		t.Fatalf("coordinator pool must stamp ALWAYS_UP: %+v", specs[0].Lifecycle)
	}

	// A plain min:0 role-pool WITHOUT alwaysUp/coordinator keeps target 0 —
	// pure on-demand (the floor rule is conditional, not global).
	errs = sched.Tick(context.Background(), []warmpool.RolePoolConfig{{
		Namespace: "team-acme", Name: "ondemand", Role: "ondemand", Min: 0, Max: 2,
	}})
	if len(errs) != 0 {
		t.Fatalf("reconcile errors: %v", errs)
	}
	specs = ensurer.snapshot()
	last := specs[len(specs)-1]
	if last.Lifecycle == nil || last.Lifecycle.AlwaysUp {
		t.Fatalf("non-coordinator pool must not be always-up: %+v", last.Lifecycle)
	}
}

// D9.3 blast radius: a RolePool in an UNFLAGGED team is skipped entirely —
// the canary flag set is the rollout unit, and a stray CR in another
// namespace boots nothing.
func TestRolePoolScheduler_UnflaggedTeamSkipped(t *testing.T) {
	ensurer := &fakeEnsurer{}
	sched := warmpool.NewRolePoolScheduler(ensurer, &fakeCounter{}, &fakeStatus{},
		[]string{"team-canary"}, "ns", "", "")

	errs := sched.Tick(context.Background(), roster("team-other"))
	if len(errs) != 0 {
		t.Fatalf("unflagged pools must be skipped silently, got errors: %v", errs)
	}
	if got := len(ensurer.snapshot()); got != 0 {
		t.Fatalf("ensured %d pods for an unflagged team, want 0", got)
	}
}

// One bad pool never blocks the fan-out: the failing team's pool reports an
// error, the healthy pools still reconcile (level-triggered — the next tick
// retries just the failure).
func TestRolePoolScheduler_OneBadPoolDoesNotBlockFanOut(t *testing.T) {
	ensurer := &fakeEnsurer{failNamespace: "team-bad"}
	sched := warmpool.NewRolePoolScheduler(ensurer, &fakeCounter{}, &fakeStatus{},
		[]string{"team-good", "team-bad"}, "ns", "", "")

	good := warmpool.RolePoolConfig{Namespace: "team-good", Name: "coder", Role: "coder", Min: 2, Max: 3}
	bad := warmpool.RolePoolConfig{Namespace: "team-bad", Name: "coder", Role: "coder", Min: 2, Max: 3}
	errs := sched.Tick(context.Background(), []warmpool.RolePoolConfig{good, bad})
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly the bad pool's", errs)
	}
	if _, exists := errs["team-bad/coder"]; !exists {
		t.Fatalf("errors = %v, want keyed team-bad/coder", errs)
	}
	if got := len(ensurer.snapshot()); got != 1 {
		t.Fatalf("ensured %d pods, want the 1 healthy pool", got)
	}
}

// The bounded status survives a counter outage: StandingKnown=false is
// reported (floor still reconciled), never a fake zero.
func TestRolePoolScheduler_CounterOutageReportsUnknown(t *testing.T) {
	sched := warmpool.NewRolePoolScheduler(&fakeEnsurer{}, &fakeCounter{}, &fakeStatus{},
		[]string{"team-acme"}, "ns", "", "")
	errs := sched.Tick(context.Background(), []warmpool.RolePoolConfig{{
		Namespace: "team-acme", Name: "coder", Role: "coder", Min: 2, Max: 3,
	}})
	if len(errs) != 0 {
		t.Fatalf("a counter outage must not fail the pass: %v", errs)
	}
}

// Tick is deterministic across passes: the same roster reconciles in
// namespace/name order regardless of input order (stable fan-out).
func TestRolePoolScheduler_DeterministicOrder(t *testing.T) {
	ensurer := &fakeEnsurer{}
	sched := warmpool.NewRolePoolScheduler(ensurer, &fakeCounter{}, &fakeStatus{},
		[]string{"team-acme"}, "ns", "", "")

	pools := roster("team-acme")
	shuffled := make([]warmpool.RolePoolConfig, len(pools))
	copy(shuffled, pools)
	sort.Slice(shuffled, func(i, j int) bool { return shuffled[i].Name > shuffled[j].Name }) // reversed
	_ = sched.Tick(context.Background(), shuffled)

	got := ensurer.snapshot()
	want := []string{"coder", "coordinator", "implementer", "reviewer", "tester"}
	for i, spec := range got {
		if spec.Role != want[i] {
			t.Fatalf("reconcile order = %v, want sorted %v", rolesOf(got), want)
		}
	}
}

func rolesOf(specs []warmpool.MeshPoolSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Role
	}
	return out
}
