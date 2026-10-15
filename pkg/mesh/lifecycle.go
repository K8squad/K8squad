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

package mesh

import (
	"context"
	"time"
)

// ProcState is one standing process's lifecycle state (chapter §6, D4.1):
//
//	spawn ──▶ IDLE ◀── task done ── ACTIVE (routed task live)
//	            │
//	            │ hibernateToStop idle bound (no run)
//	            ▼
//	          STOPPED ── next A2A request ──▶ Recreate from envelope (≤3 s)
//
// REV-4 reconciliation (ISI-5807 X1/M4, locked f93795d5): HIBERNATED is CUT
// from the P1 batch path — the SIGSTOP/SIGCONT tier is the single
// highest-hazard increment and ships LAST at strangler step S4 (ISI-5831),
// carrying the claim-never-frozen invariant (D9.1). P1.2 exercises ONLY the
// IDLE and STOPPED/recreate tiers (recreate-from-envelope is the real RAM
// reclaim). StateHibernated exists so the S4 state machine inherits the enum,
// the metrics label set and the knob surface unchanged; the transition itself
// is dark — the gateway NEVER drives a process into it in P1.
type ProcState string

const (
	// StateActive: the process is executing a routed task (busy).
	StateActive ProcState = "active"
	// StateIdle: the process is standing, routable, and accruing idle time
	// toward the stop bound. A fresh spawn enters here (routable immediately —
	// the P1.1 selection "prefer idle" treats it as the first pick).
	StateIdle ProcState = "idle"
	// StateHibernated: SIGSTOP'd, resident, instant wake — S4 (ISI-5831).
	// Dark in P1: no gateway transition enters this state (REV-4).
	StateHibernated ProcState = "hibernated"
	// StateStopped: terminated — RAM reclaimed. The slot stays on the gateway
	// so the next A2A request recreates it from the envelope (the ≤3 s
	// near-bare-exec path); the old Handle is stale.
	StateStopped ProcState = "stopped"
)

// String implements the metric-label form (bounded set: active|idle|hibernated|
// stopped — safe as a ksquad.mesh.processes.active label).
func (s ProcState) String() string { return string(s) }

// LifecycleConfig is the per-pool lifecycle knob set (chapter §6.2, D4.2). All
// four knobs ultimately live on RolePool.spec.lifecycle with operator-wide
// defaults behind them (the KSQUAD_MESH_LIFECYCLE_* env pattern, mirroring
// KSQUAD_WARM_POOL_TARGET); until the RolePool CRD lands (P1.3, ISI-5824) the
// operator defaults are the whole surface. The GATEWAY owns the clock — it runs
// the CheckInterval sweep over its own processes; the Supervisor only reacts to
// the resulting state transitions, keeping the control plane off the
// per-process hot path.
type LifecycleConfig struct {
	// CheckInterval is the gateway's regular-pace state-sweep cadence
	// (default 30s, mirroring the cphealth 15–30s cadence).
	CheckInterval time.Duration
	// IdleToHibernate bounds idle time before SIGSTOP hibernation (default
	// 5m). S4-RESERVED (ISI-5831): parsed and plumbed so the spec surface is
	// complete, but NO P1 transition consumes it — hibernation is cut from
	// the batch path (REV-4) and idle time runs straight to the stop bound.
	IdleToHibernate time.Duration
	// HibernateToStop bounds how long a process may sit idle (or, at S4,
	// hibernated) with no run before the gateway terminates it (default 4h).
	// This is the real RAM reclaim: recreate-from-envelope on the next
	// request is the ≤3s path back.
	HibernateToStop time.Duration
	// AlwaysUp exempts the pool from stop (and, at S4, hibernate): the
	// coordinator role — pinned ACTIVE/IDLE, floor 1, never scaled away.
	AlwaysUp bool
}

// Lifecycle defaults (chapter §6.2 D4.2 table).
const (
	DefaultCheckInterval   = 30 * time.Second
	DefaultIdleToHibernate = 5 * time.Minute
	DefaultHibernateToStop = 4 * time.Hour
)

// WithDefaults fills unset knobs with the operator defaults (D4.2 table).
func (c LifecycleConfig) WithDefaults() LifecycleConfig {
	if c.CheckInterval <= 0 {
		c.CheckInterval = DefaultCheckInterval
	}
	if c.IdleToHibernate <= 0 {
		c.IdleToHibernate = DefaultIdleToHibernate
	}
	if c.HibernateToStop <= 0 {
		c.HibernateToStop = DefaultHibernateToStop
	}
	return c
}

// RouteSelection is how a route picked its process (chapter §8.2
// ksquad.mesh.selection) — a bounded three-value set.
type RouteSelection string

const (
	// SelectionIdle: an existing idle standing process took the task.
	SelectionIdle RouteSelection = "idle"
	// SelectionOverflow: no idle process stood, so a stopped slot was
	// recreated from the envelope on demand (the RAM-reclaim trade: reclaim
	// when cold, pay the recreate on the next request).
	SelectionOverflow RouteSelection = "overflow"
	// SelectionWake: a hibernated process was woken. S4 only (ISI-5831) —
	// never produced in P1; part of the set so the S4 transition changes no
	// schema.
	SelectionWake RouteSelection = "wake"
)

// SpawnSource is where a spawn came from (ksquad.mesh.spawn.source).
type SpawnSource string

const (
	// SpawnSourceEnvelope: recreate/rebuild from the pre-loaded role envelope
	// — near-bare-exec into already-staged image/tools, the ≤3s target path.
	SpawnSourceEnvelope SpawnSource = "envelope"
	// SpawnSourceCold: the gateway's pool-boot spawn (gateway start).
	SpawnSourceCold SpawnSource = "cold"
)

// LifecycleObserver is the gateway's lifecycle telemetry seam: the generic
// mesh package reports route/spawn events WITH timing; the host side (k8squad:
// pkg/telemetry/meshobs) turns them into mesh.route/mesh.spawn spans and
// ksquad.mesh.* metrics. Keeping the seam here — instead of an OTel import in
// the generic gateway — preserves the extraction contract (package mesh stays
// free of any telemetry SDK; another AgentMesh adopter plugs its own observer).
//
// All hooks are informational and MUST NOT block or fail the lifecycle: an
// observer error is the host's problem, never the mesh's.
type LifecycleObserver interface {
	// ObserveRoute reports one routing decision: A2A arrival → process-ready,
	// the ≤3s recreate-SLO window. ctx carries the dispatch request's trace
	// context so the observer's span nests under the run's trace (§8.2).
	ObserveRoute(ctx context.Context, e RouteEvent)
	// ObserveSpawn reports one spawn/recreate with its wall-clock duration and
	// source. On the recreate path ctx is the triggering route's context, so
	// the spawn span nests under mesh.route.
	ObserveSpawn(ctx context.Context, e SpawnEvent)
}

// RouteEvent is one observed routing decision.
type RouteEvent struct {
	// Team, Role, Pod, EndpointID are the bounded routing identity of the
	// selected process (span attributes; also the metric label set).
	Team       string
	Role       string
	Pod        string
	EndpointID string
	// Selection says how the process was picked (idle|overflow|wake).
	Selection RouteSelection
	// Duration is arrival→ready wall-clock — the recreate SLO window when
	// Selection is overflow.
	Duration time.Duration
	// Err is non-nil when the route failed.
	Err error
}

// SpawnEvent is one observed spawn or recreate.
type SpawnEvent struct {
	Team       string
	Role       string
	Pod        string
	EndpointID string
	// Source is envelope (recreate) or cold (pool boot).
	Source SpawnSource
	// Duration is the spawn's wall-clock time — the ≤3s recreate target when
	// Source is envelope.
	Duration time.Duration
	// Err is non-nil when the spawn failed.
	Err error
}
