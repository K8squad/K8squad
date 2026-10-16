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

// scheduling.go — the mesh SCHEDULING seam (leak L3, ISI-5827 P3.5, chapter
// §4 D2.2): the third pluggable surface beside RuntimeAdapter (mechanism) and
// PlatformHost (substrate). The chapter's "role scheduling becomes a POLICY"
// has no seam in the host — k8squad's RolePool target-N prescheduling owned
// the loop outright (list CRs, sort, ensure, read back, report). Here the
// LOOP is the mesh's and the DECISIONS are the policy's: a SchedulingPolicy
// resolves the prescheduling inputs — the role → target-N band (floor +
// saturation ceiling) and the standing envelope — for every pool the mesh
// should hold this pass, and the generic Scheduler reconciles those inputs
// toward live standing capacity, level-triggered, one pass per Tick.
//
// k8squad's RolePool is policy #1: its CR lister resolves RolePool →
// (Role × AgentRuntime) facts into PoolPlans (band floored by the
// coordinator/always-up rule, envelope pre-loaded) and its provisioner boots
// one standing gateway pod per plan. A second host expresses its own
// declarative source (a YAML roster, an autoscaler signal) as another
// policy without the loop changing — which is the point of the seam.
//
// Deliberate shape choices, mirroring the host loop this lifts:
//
//   - the policy OWNS the floor rules (coordinator floor, always-up
//     exemption, team-flag gating): PoolPlan.Min arrives already resolved, so
//     the scheduler never second-guesses a policy's math;
//   - one Tick = one pass over the full resolved set, deterministic
//     team/name order so fan-out is stable across ticks;
//   - pools reconcile independently — one bad pool never blocks the fan-out;
//     the per-pool error rides the map keyed "team/name", retried next tick;
//   - the standing read-back is bounded (a count, never a per-process list),
//     and a counter outage degrades to StandingKnown=false rather than
//     failing the pass — the floor is still reconciled.
//
// This file stays [generic] by construction: no cluster API, no CRD, no host
// package — the substrate arrives as the three mechanism seams below.
package mesh

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// PoolPlan is one resolved standing pool's prescheduling decision — the
// generic "role → target-N band + envelope" input a SchedulingPolicy
// produces and the Scheduler reconciles. Team is the isolation boundary the
// pool serves (k8squad: the team namespace); Name is the declarative unit's
// own name (k8squad: the RolePool CR name) used for status write-back and
// error keying; Role is the role the pool serves.
type PoolPlan struct {
	Team string
	Name string
	Role string
	// Envelope is the resolved standing envelope pre-loaded into the pool
	// (runtime type, model, skills). The policy resolves references; the
	// scheduler passes it through opaquely.
	Envelope Envelope
	// Min is the target-N floor — the standing count the pool must hold
	// (policy-floored: the coordinator/always-up rule is applied by the
	// policy, never re-derived here).
	Min int
	// Max is the saturation ceiling of the standing band; Max <= Min means
	// no growth band (the single-floor shape).
	Max int
	// Lifecycle is the per-pool lifecycle knob set (chapter §6.2 D4.2).
	// nil keeps the gateway's operator defaults.
	Lifecycle *LifecycleConfig
	// Generation is the declarative source's generation, stamped on the
	// status read-back (bookkeeping).
	Generation int64
}

// SchedulingPolicy is the prescheduling seam (leak L3): it decides WHAT
// standing capacity the mesh should hold — which pools exist this pass, and
// per pool the target-N band and the resolved envelope. The Scheduler owns
// the reconcile loop and asks the policy for the plan set on every Tick;
// policies are free to re-resolve declarative sources per pass (dynamic
// keys appear and disappear at runtime).
type SchedulingPolicy interface {
	// ResolvePools returns the standing pools to reconcile this pass. A
	// returned error fails the whole Tick (the policy's source is
	// unreadable); per-pool reference problems are the policy's to skip or
	// report — a half-resolved pool must never reach the scheduler.
	ResolvePools(ctx context.Context) ([]PoolPlan, error)
}

// PoolProvisioner is the substrate seam of scheduling: it stands up (or
// confirms) the durable unit one pool runs inside — idempotent
// create-if-missing, so repeated Ticks never wedge. k8squad boots one
// standing gateway pod per plan; another host may provision a VM, a
// container, or an in-process gateway.
type PoolProvisioner interface {
	EnsurePool(ctx context.Context, plan PoolPlan) error
}

// StandingCounter reports the LIVE standing count for one (team, role)
// pool — the bounded read-back the status write uses. It must never return
// a per-process list, only the count.
type StandingCounter interface {
	StandingProcesses(ctx context.Context, team, role string) (int, error)
}

// PoolStatusReport is the bounded status read-back shape a PoolStatusWriter
// persists. StandingKnown=false means the live-count read failed this pass —
// the floor is still reconciled, the count is simply not fresh
// (level-triggered: the next tick retries).
type PoolStatusReport struct {
	Standing           int32
	StandingKnown      bool
	Target             int32
	ObservedGeneration int64
}

// PoolStatusWriter persists one pool's bounded report. The host owns the
// sink (a CRD status subresource, a metrics label set); the report shape is
// the scheduler's.
type PoolStatusWriter interface {
	WritePoolStatus(ctx context.Context, plan PoolPlan, report PoolStatusReport) error
}

// Scheduler is the generic target-N prescheduling loop: each Tick it asks
// the policy for the plan set, then reconciles every plan — provision the
// durable unit, read back the live standing count, write the bounded
// status. Level-triggered: a failed pool is retried on the next tick; a
// resolved-away pool is simply absent from the next plan set.
type Scheduler struct {
	policy      SchedulingPolicy
	provisioner PoolProvisioner
	counter     StandingCounter
	status      PoolStatusWriter
}

// NewScheduler assembles the scheduler. counter and status may be nil — a
// host wiring only the provisioning half gets no read-back and no status
// write rather than a panic.
func NewScheduler(policy SchedulingPolicy, provisioner PoolProvisioner, counter StandingCounter, status PoolStatusWriter) *Scheduler {
	return &Scheduler{policy: policy, provisioner: provisioner, counter: counter, status: status}
}

// Tick runs one reconcile pass over the policy-resolved plan set
// (deterministic order: sorted by team/name, so fan-out is stable across
// ticks). The returned error is a whole-pass resolution failure; per-pool
// reconcile failures ride the map keyed "team/name" so one bad pool never
// blocks the fan-out.
func (s *Scheduler) Tick(ctx context.Context) (map[string]error, error) {
	plans, err := s.policy.ResolvePools(ctx)
	if err != nil {
		return nil, err
	}

	sorted := make([]PoolPlan, len(plans))
	copy(sorted, plans)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Team != sorted[j].Team {
			return sorted[i].Team < sorted[j].Team
		}
		return sorted[i].Name < sorted[j].Name
	})

	out := map[string]error{}
	for _, plan := range sorted {
		if err := s.reconcileOne(ctx, plan); err != nil {
			out[plan.Team+"/"+plan.Name] = err
		}
	}
	return out, nil
}

// clampToInt32 bounds an int before the int32 conversion so a bogus count
// can't wrap negative (gosec G115 — the codebase clamp pattern, cf.
// jetstream/publisher.go ConsumerLag). Standing/target are process counts
// bounded far below MaxInt32 in practice; the clamp only guards the status
// write against a hostile counter.
func clampToInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	// #nosec G115 -- n is bounded by the clamp above; gosec's range
	// analysis does not track the guard. Process counts never approach
	// MaxInt32 — the clamp is belt-and-suspenders for the status write.
	return int32(n)
}

// reconcileOne is the per-pool target-N pass — the same shape as the
// warm-pool base-stock loop (live vs target, scale-up immediate), one key
// per pool:
//
//	plan   = the POLICY's resolved inputs (band + envelope, floor applied)
//	unit   = provisioner.EnsurePool (idempotent create-if-missing)
//	status = bounded count read-back, never a list
func (s *Scheduler) reconcileOne(ctx context.Context, plan PoolPlan) error {
	if err := s.provisioner.EnsurePool(ctx, plan); err != nil {
		return fmt.Errorf("ensure pool: %w", err)
	}

	report := PoolStatusReport{
		Target:             clampToInt32(plan.Min),
		ObservedGeneration: plan.Generation,
	}
	if s.counter != nil {
		standing, err := s.counter.StandingProcesses(ctx, plan.Team, plan.Role)
		if err == nil {
			report.Standing = clampToInt32(standing)
			report.StandingKnown = true
		} // read failure: report without the count — status-only signal, next tick retries
	}
	if s.status != nil {
		if err := s.status.WritePoolStatus(ctx, plan, report); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
	}
	return nil
}
