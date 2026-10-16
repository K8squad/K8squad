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

// rolepool.go — the AgentMesh P1.3 prescheduling POLICY #1 (chapter §4 D2.2,
// ISI-5824; P3.5 leak L3, ISI-6038): the RolePool target-N prescheduling
// moved from OWNING the reconcile loop to implementing the mesh scheduling
// seam. The generic target-N loop (deterministic fan-out, level-triggered
// per-pool reconcile, bounded status read-back) is the module's
// mesh.Scheduler; this file is the policy that feeds it — it resolves each
// RolePool CR's prescheduling inputs (role → target-N band, envelope) into
// generic mesh.PoolPlans:
//
//   - standing.min/max counts gateway-managed PROCESSES, not pods (the
//     ISI-5812 DevOps verdict): the scheduler's provisioner reconciles the
//     pool POD carrying the band (BootMeshPool, idempotent), and the in-pod
//     gateway enforces the process count itself (spawns Min at start,
//     grows toward Max on saturation, shrinks grown slots on idle). The pod
//     IS the durable unit the operator owns; process spawn has no
//     operator-facing verb by design (the RuntimeAdapter lives in-pod,
//     chapter §2.5).
//   - dynamic keys: RolePools appear and disappear at runtime, so the policy
//     re-resolves the listed set every pass (the mesh twin of the DevOps
//     "dynamic key registration" seam).
//
// Coordinator floor (D2.2): target = max(standing.min, 1) when the pool's
// Role is a Coordinator or lifecycle.alwaysUp is set — the always-up process
// that drives the team is exempt from the idle→stop sweep (stamped as the
// pod's ALWAYS_UP env, which the gateway honors). The floor is applied HERE,
// in the policy — the scheduler never re-derives a policy's math.
//
// Saturation (D2.2/§5.3): the growth reaction is in-pod (the gateway sees
// saturation at route time and grows toward Max); the policy's job here is
// the floor, the band, and the envelope. The bounded status read-back is
// the scheduler's (mesh.PoolStatusReport), persisted host-side through the
// CRD status subresource.
package warmpool

import (
	"context"
	"time"

	agentmesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
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
	// RuntimeType is spec.runtimeRef's resolved AgentRuntime.spec.type —
	// the coding-agent flavor the pool's children run. The substrate
	// resolves it to a sandbox image (KubeEnsurer.ImageFor); the
	// composition root binds the matching RuntimeAdapter from it (the P2
	// close-out follow-up, ISI-5826 comment b09e1970).
	RuntimeType string
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

// RolePoolLister resolves the cluster's RolePool CRs into scheduler-ready
// RolePoolConfigs: for each CR it dereferences spec.roleRef (model, skills,
// coordinator) and spec.runtimeRef (runtime type). A CR whose references do
// not resolve is reported in the unresolved map keyed "namespace/name" — a
// half-configured pool must never reach the scheduler, and must never block
// the pools that do resolve. (Production: CRLister, rolepool_prod.go.)
type RolePoolLister interface {
	ListRolePools(ctx context.Context) ([]RolePoolConfig, map[string]error, error)
}

// RolePoolPolicy is SchedulingPolicy implementation #1: it turns the
// cluster's RolePool CRs into the mesh's generic prescheduling inputs — one
// mesh.PoolPlan per resolved (team, role) pool carrying the target-N band
// (floor already coordinator-floored) and the resolved standing envelope.
// The mesh.Scheduler consumes this seam and nothing else: RolePool drives
// the mesh through the policy interface only (ISI-6038 acceptance).
type RolePoolPolicy struct {
	lister RolePoolLister
	teams  map[string]struct{} // D9.3: only flagged teams reconcile
	// onUnresolved, when set, is told about every half-configured CR the
	// lister could not resolve (keyed "namespace/name") — the operator
	// wires it to its log; the pool is skipped either way.
	onUnresolved func(key string, err error)
}

// NewRolePoolPolicy assembles the policy. teams is the D9.3 canary set —
// a RolePool in an unflagged namespace is inert (blast radius: the canary
// set only).
func NewRolePoolPolicy(lister RolePoolLister, teams []string, onUnresolved func(key string, err error)) *RolePoolPolicy {
	set := make(map[string]struct{}, len(teams))
	for _, t := range teams {
		set[t] = struct{}{}
	}
	return &RolePoolPolicy{lister: lister, teams: set, onUnresolved: onUnresolved}
}

// ResolvePools lists and resolves the cluster's RolePools into the generic
// plan set (dynamic keys: whatever exists this pass). Unresolved references
// are skipped (reported through onUnresolved — a half-configured pool must
// never boot a wrong-shaped pod); unflagged teams are skipped (D9.3); a
// listing failure fails the pass whole.
func (p *RolePoolPolicy) ResolvePools(ctx context.Context) ([]agentmesh.PoolPlan, error) {
	pools, unresolved, err := p.lister.ListRolePools(ctx)
	if err != nil {
		return nil, err
	}
	for key, uerr := range unresolved {
		if p.onUnresolved != nil {
			p.onUnresolved(key, uerr)
		}
	}

	plans := make([]agentmesh.PoolPlan, 0, len(pools))
	for _, rp := range pools {
		if _, flagged := p.teams[rp.Namespace]; !flagged {
			continue // not a mesh team (D9.3): the CR is inert until flagged
		}
		plans = append(plans, planForRolePool(rp))
	}
	return plans, nil
}

// planForRolePool converts one resolved RolePool into the mesh's generic
// prescheduling input: the target-N band (floor coordinator-floored here,
// ceiling as declared) and the standing envelope (runtime type, model,
// skills — the D2.3 pre-load facts).
func planForRolePool(rp RolePoolConfig) agentmesh.PoolPlan {
	return agentmesh.PoolPlan{
		Team: rp.Namespace,
		Name: rp.Name,
		Role: rp.Role,
		Envelope: agentmesh.Envelope{
			RuntimeType: rp.RuntimeType,
			Model:       rp.Model,
			Skills:      rp.Skills,
		},
		Min: rp.StandingTarget(),
		Max: rp.Max,
		Lifecycle: &agentmesh.LifecycleConfig{
			CheckInterval:   durationOrZero(rp.Lifecycle.CheckInterval),
			IdleToHibernate: durationOrZero(rp.Lifecycle.IdleToHibernate),
			HibernateToStop: durationOrZero(rp.Lifecycle.HibernateToStop),
			AlwaysUp:        rp.AlwaysUp(),
		},
		Generation: rp.Generation,
	}
}

// durationOrZero parses one CRD lifecycle knob (a duration string). Empty
// or malformed yields 0 — the gateway's default cadence — so a bad knob
// degrades to the default, never to a wrong-shaped pod.
func durationOrZero(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}
