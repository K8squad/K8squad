/*
Copyright 2026 The K8sAgentMesh Authors.

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

package mesh_test

// scheduling_test.go — the generic scheduling-loop pins (leak L3, ISI-5827
// P3.5): the 12/5→2–3 math through the seam (five role pools at target-N
// 2–3 fan out to five ensured units, floors summing to ~12 standing
// processes), the per-pool error fan-out, the bounded status read-back with
// its counter-outage degradation, and deterministic pass ordering. The
// policy-side pins (coordinator floor, team-flag gating, CR resolution) are
// host-side: they live with policy #1 (k8squad pkg/warmpool).

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	mesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// fakePolicy serves a fixed plan set (or a resolution error).
type fakePolicy struct {
	plans []mesh.PoolPlan
	err   error
}

func (f *fakePolicy) ResolvePools(context.Context) ([]mesh.PoolPlan, error) {
	return f.plans, f.err
}

// fakeProvisioner records the plans the scheduler asked to stand up.
type fakeProvisioner struct {
	mu    sync.Mutex
	plans []mesh.PoolPlan
	// failTeam fails every ensure for this team (blast-radius probe).
	failTeam string
}

func (f *fakeProvisioner) EnsurePool(_ context.Context, plan mesh.PoolPlan) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failTeam != "" && plan.Team == f.failTeam {
		return errors.New("provisioner: injected failure")
	}
	f.plans = append(f.plans, plan)
	return nil
}

func (f *fakeProvisioner) snapshot() []mesh.PoolPlan {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]mesh.PoolPlan, len(f.plans))
	copy(out, f.plans)
	return out
}

// fakeCounter serves per-(team, role) standing counts; nil counts error.
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

// fakeStatus records the bounded reports written per pool.
type fakeStatus struct {
	mu      sync.Mutex
	written map[string]mesh.PoolStatusReport
	failKey string
}

func (f *fakeStatus) WritePoolStatus(_ context.Context, plan mesh.PoolPlan, report mesh.PoolStatusReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failKey != "" && plan.Team+"/"+plan.Name == f.failKey {
		return errors.New("status: injected failure")
	}
	if f.written == nil {
		f.written = map[string]mesh.PoolStatusReport{}
	}
	f.written[plan.Team+"/"+plan.Name] = report
	return nil
}

func (f *fakeStatus) get(key string) (mesh.PoolStatusReport, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.written[key]
	return r, ok
}

// roster is the proving roster: FIVE roles at target-N 2–3 each — the
// "12 agents / 5 roles → 2–3 per pool" shape (chapter §9.1/§2.3), with the
// coordinator's floor ≥ 1 already applied by the policy.
func roster(team string) []mesh.PoolPlan {
	mk := func(name string, min, max int) mesh.PoolPlan {
		return mesh.PoolPlan{
			Team: team,
			Name: name,
			Role: name,
			Envelope: mesh.Envelope{
				RuntimeType: "opencode",
				Model:       "glm-4.7",
			},
			Min:        min,
			Max:        max,
			Generation: 1,
		}
	}
	return []mesh.PoolPlan{
		mk("coordinator", 2, 2),
		mk("coder", 3, 3),
		mk("reviewer", 2, 3),
		mk("implementer", 2, 3),
		mk("tester", 3, 3),
	}
}

// THE 12/5→2–3 pin: five plans reconcile into five ensured units, each
// carrying its declared band, the floors summing to exactly 12 standing
// processes across the team — and every pool gets its bounded status with
// the target and the counter's standing count.
func TestScheduler_12Over5Math(t *testing.T) {
	provisioner := &fakeProvisioner{}
	status := &fakeStatus{}
	counter := &fakeCounter{counts: map[string]int{}}
	plans := roster("team-acme")
	sched := mesh.NewScheduler(&fakePolicy{plans: plans}, provisioner, counter, status)

	for _, p := range plans {
		counter.counts["team-acme/"+p.Role] = p.Min // registry mirror reports the floors standing
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
		if plan.Envelope.RuntimeType == "" || plan.Envelope.Model == "" {
			t.Fatalf("pool %s missing resolved envelope: %+v", plan.Role, plan.Envelope)
		}
		sum += plan.Min
	}
	if sum != 12 {
		t.Fatalf("standing floors sum to %d across 5 roles, want 12", sum)
	}

	// Every pool got its bounded status with the target and live count.
	for _, p := range plans {
		report, ok := status.get("team-acme/" + p.Name)
		if !ok {
			t.Fatalf("no status written for %s", p.Name)
		}
		if report.Target != int32(p.Min) {
			t.Fatalf("status target = %d, want %d", report.Target, p.Min)
		}
		if !report.StandingKnown || report.Standing != int32(p.Min) {
			t.Fatalf("status standing = %d (known=%v), want the counter's %d", report.Standing, report.StandingKnown, p.Min)
		}
		if report.ObservedGeneration != 1 {
			t.Fatalf("status generation = %d, want 1", report.ObservedGeneration)
		}
	}
}

// One bad pool never blocks the fan-out: the failing team's pool reports an
// error, the healthy pools still reconcile (level-triggered — the next tick
// retries just the failure).
func TestScheduler_OneBadPoolDoesNotBlockFanOut(t *testing.T) {
	provisioner := &fakeProvisioner{failTeam: "team-bad"}
	good := mesh.PoolPlan{Team: "team-good", Name: "coder", Role: "coder", Min: 2, Max: 3}
	bad := mesh.PoolPlan{Team: "team-bad", Name: "coder", Role: "coder", Min: 2, Max: 3}
	sched := mesh.NewScheduler(&fakePolicy{plans: []mesh.PoolPlan{good, bad}}, provisioner, &fakeCounter{}, &fakeStatus{})

	errs, rerr := sched.Tick(context.Background())
	if rerr != nil {
		t.Fatalf("resolve error: %v", rerr)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly the bad pool's", errs)
	}
	if _, exists := errs["team-bad/coder"]; !exists {
		t.Fatalf("errors = %v, want keyed team-bad/coder", errs)
	}
	if got := len(provisioner.snapshot()); got != 1 {
		t.Fatalf("ensured %d pools, want the 1 healthy pool", got)
	}
}

// The bounded status survives a counter outage: StandingKnown=false is
// reported (floor still reconciled), never a fake zero, and the pass does
// not fail.
func TestScheduler_CounterOutageReportsUnknown(t *testing.T) {
	provisioner := &fakeProvisioner{}
	status := &fakeStatus{}
	sched := mesh.NewScheduler(&fakePolicy{plans: []mesh.PoolPlan{{
		Team: "team-acme", Name: "coder", Role: "coder", Min: 2, Max: 3,
	}}}, provisioner, &fakeCounter{}, status)

	errs, rerr := sched.Tick(context.Background())
	if rerr != nil {
		t.Fatalf("resolve error: %v", rerr)
	}
	if len(errs) != 0 {
		t.Fatalf("a counter outage must not fail the pass: %v", errs)
	}
	report, ok := status.get("team-acme/coder")
	if !ok {
		t.Fatalf("no status written")
	}
	if report.StandingKnown || report.Standing != 0 {
		t.Fatalf("counter outage must report StandingKnown=false, got %+v", report)
	}
	if report.Target != 2 {
		t.Fatalf("counter outage must still report the target, got %+v", report)
	}
}

// A status-write failure is a per-pool error in the map — the next tick
// retries it; other pools are unaffected.
func TestScheduler_StatusWriteFailureIsPerPoolError(t *testing.T) {
	provisioner := &fakeProvisioner{}
	status := &fakeStatus{failKey: "team-acme/coder"}
	sched := mesh.NewScheduler(&fakePolicy{plans: []mesh.PoolPlan{
		{Team: "team-acme", Name: "coder", Role: "coder", Min: 2, Max: 3},
		{Team: "team-acme", Name: "tester", Role: "tester", Min: 1, Max: 1},
	}}, provisioner, &fakeCounter{}, status)

	errs, rerr := sched.Tick(context.Background())
	if rerr != nil {
		t.Fatalf("resolve error: %v", rerr)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly the failing status write", errs)
	}
	if _, exists := errs["team-acme/coder"]; !exists {
		t.Fatalf("errors = %v, want keyed team-acme/coder", errs)
	}
}

// A policy resolution failure fails the whole Tick and reconciles nothing —
// the policy's source is unreadable, so no pool can be trusted this pass.
func TestScheduler_PolicyResolveErrorFailsTick(t *testing.T) {
	provisioner := &fakeProvisioner{}
	sched := mesh.NewScheduler(&fakePolicy{err: errors.New("source: down")}, provisioner, &fakeCounter{}, &fakeStatus{})

	_, rerr := sched.Tick(context.Background())
	if rerr == nil {
		t.Fatalf("resolve error must fail the tick")
	}
	if got := len(provisioner.snapshot()); got != 0 {
		t.Fatalf("ensured %d pools on a failed resolve, want 0", got)
	}
}

// Tick is deterministic across passes: the same roster reconciles in
// team/name order regardless of the policy's output order (stable fan-out).
func TestScheduler_DeterministicOrder(t *testing.T) {
	provisioner := &fakeProvisioner{}

	plans := roster("team-acme")
	shuffled := make([]mesh.PoolPlan, len(plans))
	copy(shuffled, plans)
	sort.Slice(shuffled, func(i, j int) bool { return shuffled[i].Name > shuffled[j].Name }) // reversed
	sched := mesh.NewScheduler(&fakePolicy{plans: shuffled}, provisioner, &fakeCounter{}, &fakeStatus{})
	errs, _ := sched.Tick(context.Background())
	if len(errs) != 0 {
		t.Fatalf("reconcile errors: %v", errs)
	}

	got := provisioner.snapshot()
	want := []string{"coder", "coordinator", "implementer", "reviewer", "tester"}
	for i, plan := range got {
		if plan.Role != want[i] {
			t.Fatalf("reconcile order = %v, want sorted %v", rolesOf(got), want)
		}
	}
}

// Nil counter/status are legal wiring: the loop provisions and reports no
// read-back rather than panicking.
func TestScheduler_NilCounterAndStatusAreLegal(t *testing.T) {
	provisioner := &fakeProvisioner{}
	sched := mesh.NewScheduler(&fakePolicy{plans: roster("team-acme")}, provisioner, nil, nil)

	errs, rerr := sched.Tick(context.Background())
	if rerr != nil {
		t.Fatalf("resolve error: %v", rerr)
	}
	if len(errs) != 0 {
		t.Fatalf("reconcile errors: %v", errs)
	}
	if got := len(provisioner.snapshot()); got != 5 {
		t.Fatalf("ensured %d pools, want 5", got)
	}
}

func rolesOf(plans []mesh.PoolPlan) []string {
	out := make([]string, len(plans))
	for i, p := range plans {
		out[i] = p.Role
	}
	return out
}
