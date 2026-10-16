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
//	            │ idleToHibernate (S4: Hibernate armed)      ┌──────────┐
//	            ▼                                          │ HIBERNATED│
//	          HIBERNATED ── wake on route (SIGCONT) ────────▶└──────────┘
//	            │
//	            │ hibernateToStop from the hibernate instant (no run)
//	            ▼
//	          STOPPED ── next A2A request ──▶ Recreate from envelope (≤3 s)
//
// REV-4 (ISI-5807 X1/M4, locked f93795d5) shipped P1 with HIBERNATED cut from
// the batch path; S4 (ISI-5831) arms the two-tier ladder behind
// LifecycleConfig.Hibernate — an explicit per-pool opt-in so the S4 rollout is
// reversible independently of mesh routing (D9.3). With Hibernate disarmed the
// gateway NEVER drives a process into StateHibernated: idle time accrues
// straight to the stop bound (the P1.2 collapsed ladder, byte-identical).
type ProcState string

const (
	// StateActive: the process is executing a routed task (busy).
	StateActive ProcState = "active"
	// StateIdle: the process is standing, routable, and accruing idle time
	// toward the stop bound. A fresh spawn enters here (routable immediately —
	// the P1.1 selection "prefer idle" treats it as the first pick).
	StateIdle ProcState = "idle"
	// StateHibernated: SIGSTOP'd, resident, instant wake — S4 (ISI-5831),
	// armed by LifecycleConfig.Hibernate. Dark when Hibernate is disarmed
	// (the REV-4 P1.2 collapsed ladder).
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
	// Hibernate arms the S4 two-tier ladder (ISI-5831, chapter §6.1 D4.1):
	// IDLE → (idleToHibernate) SIGSTOP → HIBERNATED → (hibernateToStop from
	// the hibernate instant) → STOPPED, and Route gains the wake tier — a
	// hibernated process is woken (SIGCONT) instead of recreated, with the
	// model rebound at the wake boundary when the envelope drifted (X5:
	// rebind-at-wake, never mid-stream). false (the default) keeps the
	// REV-4 P1.2 collapsed ladder — idle time accrues straight to the stop
	// bound and Hibernate/Wake stay dark — so S4 rolls out as an explicit,
	// reversible per-pool opt-in (D9.3: the runtime kill-switch that lets
	// ops disable hibernation without disabling mesh routing).
	Hibernate bool
	// IdleToHibernate bounds idle time before SIGSTOP hibernation (default
	// 5m). Consumed ONLY when Hibernate is armed (S4, ISI-5831).
	IdleToHibernate time.Duration
	// HibernateToStop bounds how long a process may sit idle (or, at S4,
	// hibernated) with no run before the gateway terminates it (default 4h).
	// This is the real RAM reclaim: recreate-from-envelope on the next
	// request is the ≤3s path back.
	HibernateToStop time.Duration
	// AlwaysUp exempts the pool from stop and hibernate: the coordinator
	// role — pinned ACTIVE/IDLE, floor 1, never scaled away.
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
	// SelectionWake: a hibernated process was woken (SIGCONT) — S4 only
	// (ISI-5831); produced exclusively by a Hibernate-armed pool, never by
	// the P1.2 collapsed ladder.
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
	// ObserveWake reports one hibernation wake (S4, ISI-5831): SIGCONT →
	// process-ready, nesting under the triggering mesh.route span (§8.2 —
	// mesh.wake, which backs ksquad.mesh.wake.latency). Called only by a
	// Hibernate-armed pool; the P1.2 collapsed ladder never wakes.
	ObserveWake(ctx context.Context, e WakeEvent)
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

// WakeEvent is one observed hibernation wake (S4, ISI-5831).
type WakeEvent struct {
	Team       string
	Role       string
	Pod        string
	EndpointID string
	// Duration is the wake's wall-clock time (SIGCONT → ready) — the
	// instant-wake claim of §6.1, backing ksquad.mesh.wake.latency.
	Duration time.Duration
	// Rebind is true when the wake took the rebind path — the envelope's
	// model moved while the process slept, so the wake respawned from the
	// CURRENT envelope instead of SIGCONT (X5: rebind-at-wake, never
	// mid-stream).
	Rebind bool
	// Err is non-nil when the wake failed.
	Err error
}
