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

// rolepool.go — the AgentMesh P1.3 prescheduling scheduler (chapter §4 D2.2,
// ISI-5824): the reconcile loop that consumes RolePool CRs and drives each
// (team, role) standing pool toward its declarative band. This is the same
// base-stock target-N loop the §9.2 warm-pool Controller runs
// (controller.go:22-48) — live-vs-target, scale-up immediate, warming counts
// toward live — keyed per-ROLE instead of per-PoolKey, with two unit-seam
// adaptations the ISI-5812 DevOps verdict pinned:
//
//   - standing.min/max counts gateway-managed PROCESSES, not pods: the
//     operator reconciles the pool POD carrying the band (BootMeshPool,
//     idempotent), and the in-pod gateway enforces the process count itself
//     (spawns Min at start, grows toward Max on saturation, shrinks grown
//     slots on idle — mesh/gateway.go). The pod IS the durable unit the
//     operator owns; process spawn has no operator-facing verb by design
//     (the RuntimeAdapter lives in-pod, chapter §2.5).
//   - dynamic keys: RolePools appear and disappear at runtime, so Tick
//     reconciles whatever set the lister returns each pass — the mesh twin
//     of the DevOps "dynamic key registration" seam.
//
// Coordinator floor (D2.2): target = max(standing.min, 1) when the pool's
// Role is a Coordinator or lifecycle.alwaysUp is set — the always-up process
// that drives the team is exempt from the idle→stop sweep (stamped as the
// pod's ALWAYS_UP env, which the gateway honors).
//
// Saturation (D2.2/§5.3): the growth reaction is in-pod (the gateway sees
// saturation at route time and grows toward Max); the scheduler's job here is
// the floor, the band stamp, and the bounded status read-back
// (RolePoolStatus: standing processes from coord.mesh_endpoint, never a
// per-process list — the ISI-5812 shaping nit).
package warmpool

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// RolePoolConfig is one RESOLVED RolePool — the CR's spec plus the Role and
// AgentRuntime facts the envelope needs, resolved by the lister (CR fields
// reference; resolution dereferences). Team is the CR's namespace (the
// chapter's isolation boundary: "the team namespace IS the isolation
// boundary").
type RolePoolConfig struct {
	Namespace string // CR namespace == team
	Name      string // CR name (usually the role name)
	Role      string // spec.roleRef.name
	// RuntimeType is spec.runtimeRef's resolved AgentRuntime.spec.type;
	// Image is that type's resolved sandbox image.
	RuntimeType string
	Image       string
	Min         int
	Max         int
	// Coordinator is the referenced Role's spec.coordinator flag.
	Coordinator bool
	// Model and Skills are the referenced Role's envelope facts.
	Model  string
	Skills []string
	// Lifecycle mirrors spec.lifecycle (D4.2 knobs).
	Lifecycle MeshLifecycleSpec
	// Generation is the CR's metadata.generation (status bookkeeping).
	Generation int64
}

// StandingTarget returns the pool's standing floor with the coordinator
// rule applied (chapter §4): max(min, 1) for an always-up pool (Coordinator
// Role or lifecycle.alwaysUp), plain min otherwise.
func (c RolePoolConfig) StandingTarget() int {
	t := c.Min
	if (c.Coordinator || c.Lifecycle.AlwaysUp) && t < 1 {
		t = 1
	}
	return t
}

// AlwaysUp reports whether this pool is exempt from the idle→stop sweep
// (coordinator floor / always-up, chapter §6 diagram note).
func (c RolePoolConfig) AlwaysUp() bool {
	return c.Coordinator || c.Lifecycle.AlwaysUp
}

// PoolEnsurer boots (idempotently, create-if-missing) one standing mesh pool
// pod for spec — the KubeProvisioner.BootMeshPool seam, interface'd so the
// scheduler is provable L1 against a fake.
type PoolEnsurer interface {
	EnsureMeshPool(ctx context.Context, spec MeshPoolSpec) error
}

// StandingCounter reports the LIVE standing-process count for one (team,
// role) pool — production reads coord.mesh_endpoint (LookupTeamRole); the
// scheduler only needs the count for its bounded status read-back.
type StandingCounter interface {
	StandingProcesses(ctx context.Context, team, role string) (int, error)
}

// RolePoolStatusWriter writes one RolePool's bounded status (production: the
// controller-runtime status subresource update).
type RolePoolStatusWriter interface {
	WriteRolePoolStatus(ctx context.Context, namespace, name string, status RolePoolReport) error
}

// RolePoolReport is the scheduler's status read-back shape (the CRD status
// fields the writer persists). StandingKnown=false means the live count read
// failed this pass — the floor is still reconciled, the count is simply not
// fresh (level-triggered: the next tick retries).
type RolePoolReport struct {
	Standing           int32
	StandingKnown      bool
	Target             int32
	Pod                string
	ObservedGeneration int64
}

// RolePoolScheduler reconciles a team's RolePools: for each resolved pool it
// ensures the standing gateway pod exists carrying the right envelope and
// band, then reports the live standing count back into the CR's status. One
// Tick = one pass over the full listed set (level-triggered; failed pools are
// retried on the next tick, exactly like the warm-pool Controller's Tick).
type RolePoolScheduler struct {
	ensurer PoolEnsurer
	counter StandingCounter
	status  RolePoolStatusWriter
	ingress string
	podNS   string
	workPVC string
	teams   map[string]struct{} // D9.3: only flagged teams reconcile
}

// NewRolePoolScheduler assembles the scheduler. podNS is the namespace pool
// pods boot in (the operator's sandbox namespace — NOT the team namespace:
// the provisioner's RBAC is scoped there); ingress is the Supervisor
// registration ingress stamped on the pod; workPVC the optional per-Project
// workspace PVC.
func NewRolePoolScheduler(ensurer PoolEnsurer, counter StandingCounter, status RolePoolStatusWriter, teams []string, podNS, ingress, workPVC string) *RolePoolScheduler {
	set := make(map[string]struct{}, len(teams))
	for _, t := range teams {
		set[t] = struct{}{}
	}
	return &RolePoolScheduler{ensurer: ensurer, counter: counter, status: status, teams: set, podNS: podNS, ingress: ingress, workPVC: workPVC}
}

// Tick reconciles the given resolved RolePools (deterministic order: sorted
// by namespace/name, so fan-out is stable across ticks). A pool whose team
// is not flagged is skipped (D9.3 blast radius: the canary set only). Each
// pool reconciles independently — one bad pool never blocks the fan-out; the
// per-pool error is returned in the map keyed "namespace/name".
func (s *RolePoolScheduler) Tick(ctx context.Context, pools []RolePoolConfig) map[string]error {
	sorted := make([]RolePoolConfig, len(pools))
	copy(sorted, pools)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Namespace != sorted[j].Namespace {
			return sorted[i].Namespace < sorted[j].Namespace
		}
		return sorted[i].Name < sorted[j].Name
	})

	out := map[string]error{}
	for _, rp := range sorted {
		key := rp.Namespace + "/" + rp.Name
		if _, flagged := s.teams[rp.Namespace]; !flagged {
			continue // not a mesh team (D9.3): the CR is inert until flagged
		}
		if err := s.reconcileOne(ctx, rp); err != nil {
			out[key] = err
		}
	}
	return out
}

// clampToInt32 bounds an int before the int32 conversion so a bogus count
// can't wrap negative (gosec G115 — the codebase clamp pattern, cf.
// jetstream/publisher.go ConsumerLag). Standing/target are process counts
// bounded far below MaxInt32 in practice; the clamp only guards the CRD
// status write against a hostile counter.
func clampToInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	// #nosec G115 -- n is bounded by the clamp above; gosec's range
	// analysis does not track the guard. Process counts never approach
	// MaxInt32 — the clamp is belt-and-suspenders for the CRD status write.
	return int32(n)
}

// reconcileOne is the per-role target-N pass — the same shape as the
// warm-pool Controller's Tick body (controller.go:22-48), one key per role:
//
//	target = StandingTarget(rp)          ← the POLICY (static declarative
//	                                       floor, D2.2; coordinator rule)
//	live   = pod standing? (idempotent   ← the MECHANISM state — the pod IS
//	          create-if-missing)             the durable unit (ISI-5812)
//	pod missing → Boot (scale UP immediate, warming counts toward live on
//	              the gateway's own spawn loop)
//	status ← registry standing count      ← bounded read-back, never a list
func (s *RolePoolScheduler) reconcileOne(ctx context.Context, rp RolePoolConfig) error {
	target := rp.StandingTarget()
	spec := MeshPoolSpec{
		Team:      rp.Namespace,
		Role:      rp.Role,
		Image:     rp.Image,
		Namespace: s.podNS,
		Model:     rp.Model,
		Skills:    rp.Skills,
		Min:       target,
		Max:       rp.Max,
		Lifecycle: &MeshLifecycleSpec{
			CheckInterval:   rp.Lifecycle.CheckInterval,
			IdleToHibernate: rp.Lifecycle.IdleToHibernate,
			HibernateToStop: rp.Lifecycle.HibernateToStop,
			AlwaysUp:        rp.AlwaysUp(),
		},
		IngressURL: s.ingress,
		ProjectPVC: s.workPVC,
	}
	if err := s.ensurer.EnsureMeshPool(ctx, spec); err != nil {
		return fmt.Errorf("ensure pool pod: %w", err)
	}

	report := RolePoolReport{
		Target:             clampToInt32(target),
		Pod:                MeshPoolPodName(rp.Namespace, rp.Role),
		ObservedGeneration: rp.Generation,
	}
	if s.counter != nil {
		standing, err := s.counter.StandingProcesses(ctx, rp.Namespace, rp.Role)
		if err == nil {
			report.Standing = clampToInt32(standing)
			report.StandingKnown = true
		} // read failure: report without the count — status-only signal, next tick retries
	}
	if s.status != nil {
		if err := s.status.WriteRolePoolStatus(ctx, rp.Namespace, rp.Name, report); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
	}
	return nil
}
