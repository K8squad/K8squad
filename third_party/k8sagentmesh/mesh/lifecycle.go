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

package mesh

import (
	"context"
	"fmt"
	"sync"
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
	// ObserveWake reports one hibernation wake (S4, ISI-5831): SIGCONT →
	// ready — the instant-wake claim of §6.1, backing ksquad.mesh.wake.latency.
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

// ---------------------------------------------------------------------------
// Supervisor-side gateway health (P1.4, ISI-6010; chapter §2.4, D6.1/D6.2).
//
// The GATEWAY owns its per-process lifecycle clock (D4.2, above); the
// SUPERVISOR owns the gateway's own health: when a gateway goes down (its
// heartbeat ages past the registry TTL) or stops SERVING while still beating
// (the health leg), the supervisor drains the pod's endpoints from
// coord.mesh_endpoint and recreates the pod from its envelope-stamped spec
// (D2.3: the pod spec IS the envelope carrier, so a fresh pod re-spawns the
// pool exactly like the first boot). The invariants ride the P1.2
// recreate-from-envelope path one level up:
//
//   - DRAIN FIRST, recreate second: while a recreate is in flight or failed
//     the registry holds nothing for the pod, so the route leg can never hand
//     a task to a gateway that is going away (no phantom routable, the
//     registry-level twin of P1.2's no-phantom-ACTIVE).
//   - RETRYABLE: a failed recreate parks the pod in a pending set and every
//     sweep retries until it succeeds — a wedged recreate never strands the
//     pool silently.

// EndpointHealth is the supervisor's view of one registered endpoint: the
// registry row's identity plus its last heartbeat.
type EndpointHealth struct {
	EndpointID string
	Pod        string
	Team       string
	Role       string
	// Endpoint is the advertised routable base URL (the health probe target).
	Endpoint   string
	LastSeenAt time.Time
}

// SupervisorStore is the registry seam the health loop reads and drains
// through. k8squad binds it to coord.MeshRegistry; the seam keeps this
// package free of the control-plane store (the P3 extraction contract).
type SupervisorStore interface {
	// Endpoints lists EVERY registered endpoint, live and TTL-expired alike —
	// the loop exists precisely to drain the expired rows a TTL-filtered
	// lookup would hide.
	Endpoints(ctx context.Context) ([]EndpointHealth, error)
	// DrainPod deletes every endpoint a pod registered (§2.4: drain-by-pod).
	// It must be idempotent.
	DrainPod(ctx context.Context, pod string) error
}

// PodRecreator rebuilds a drained gateway pod from its envelope-stamped spec.
// It MUST be safe to call repeatedly for the same pod — the health loop
// retries a failed recreate on every sweep.
type PodRecreator interface {
	RecreatePod(ctx context.Context, pod string) error
}

// SupervisorHealthConfig assembles the supervisor health loop. Store and
// Recreate are required; everything else has defaults.
type SupervisorHealthConfig struct {
	Store    SupervisorStore
	Recreate PodRecreator
	// Health probes one gateway's serving readiness (production: ProbeReady
	// against the advertised endpoint). nil disables the health leg — the
	// loop then reacts to TTL-expiry only.
	Health func(ctx context.Context, endpoint string) bool
	// TTL is the endpoint routability TTL (production: coord.MeshEndpointTTL,
	// 90s — three missed 30s heartbeats). An endpoint whose LastSeenAt is
	// older is expired. <= 0 defaults to 90s.
	TTL time.Duration
	// CheckInterval is the sweep cadence (default 30s, mirroring the gateway
	// heartbeat).
	CheckInterval time.Duration
	// DownThreshold is the number of consecutive failed health probes before
	// the loop drains/recreates a pod whose rows are still TTL-live (default
	// 2): one flap must not bounce a healthy pool.
	DownThreshold int
	// Sink receives drain/recreate telemetry (ops "supervisor.drain" /
	// "supervisor.recreate" / "supervisor.endpoints", handleID=pod). nil
	// uses NopSink.
	Sink TelemetrySink
	// Now is the clock (a test seam). nil = time.Now.
	Now func() time.Time
}

// SupervisorHealth is the supervisor's gateway-health loop: one sweep per
// CheckInterval, drain+recreate on gateway down or TTL-expiry. Construct with
// NewSupervisorHealth; run with Start (manager.Runnable-shaped) or drive
// SweepOnce directly (tests).
type SupervisorHealth struct {
	cfg SupervisorHealthConfig

	mu sync.Mutex
	// downStreak counts consecutive failed health probes per pod (health leg).
	downStreak map[string]int
	// pending holds pods drained but not yet successfully recreated — the
	// retry set. A pending pod is re-attempted every sweep until RecreatePod
	// succeeds, so a failed recreate never strands a pool silently.
	pending map[string]bool
}

// NewSupervisorHealth validates the config and returns the loop.
func NewSupervisorHealth(cfg SupervisorHealthConfig) (*SupervisorHealth, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("mesh.NewSupervisorHealth: Store is required")
	}
	if cfg.Recreate == nil {
		return nil, fmt.Errorf("mesh.NewSupervisorHealth: Recreate is required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 90 * time.Second
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 30 * time.Second
	}
	if cfg.DownThreshold <= 0 {
		cfg.DownThreshold = 2
	}
	if cfg.Sink == nil {
		cfg.Sink = NopSink{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &SupervisorHealth{cfg: cfg, downStreak: map[string]int{}, pending: map[string]bool{}}, nil
}

// Start runs the sweep loop until ctx is done (manager.Runnable shape). It
// sweeps ONCE immediately at boot — an operator restart inherits whatever
// state the previous loop left, and a dead gateway should not wait a full
// interval for its drain.
func (s *SupervisorHealth) Start(ctx context.Context) error {
	s.SweepOnce(ctx)
	t := time.NewTicker(s.cfg.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepOnce runs one health pass: retry pending recreates, then evaluate every
// registered pod on both legs —
//
//   - TTL leg: a pod with ZERO live endpoints missed three heartbeats; the
//     gateway is down even if nothing watched it die.
//   - Health leg: a pod with live rows whose /ready probe fails DownThreshold
//     consecutive times is beating but cannot SERVE (wedged gateway).
//
// Either trigger drains the pod's endpoints FIRST and recreates the pod
// SECOND (drain-first = the no-phantom invariant). A partially-stale pod
// (some endpoints expired, some live) is left alone: the gateway's own
// registration beat prunes its dead ids (the ingress PrunePod path), and its
// P1.2 overflow route recreates stopped slots on demand.
func (s *SupervisorHealth) SweepOnce(ctx context.Context) {
	s.retryPending(ctx)

	eps, err := s.cfg.Store.Endpoints(ctx)
	if err != nil {
		s.cfg.Sink.OnLifecycle("supervisor.endpoints", "", "", err)
		return
	}
	type podView struct {
		endpoint string // probe target: any live row's advertised URL
		live     int
	}
	pods := map[string]*podView{}
	cutoff := s.cfg.Now().Add(-s.cfg.TTL)
	for _, ep := range eps {
		v := pods[ep.Pod]
		if v == nil {
			v = &podView{}
			pods[ep.Pod] = v
		}
		if ep.LastSeenAt.After(cutoff) {
			v.live++
			if v.endpoint == "" {
				v.endpoint = ep.Endpoint
			}
		}
	}
	for pod, v := range pods {
		if s.isPending(pod) {
			continue // already drained; the retry above owns it
		}
		down := v.live == 0
		if !down && s.cfg.Health != nil && v.endpoint != "" {
			if s.cfg.Health(ctx, v.endpoint) {
				s.resetDown(pod)
			} else if s.bumpDown(pod) >= s.cfg.DownThreshold {
				down = true
			}
		}
		if !down {
			continue
		}
		s.drainRecreate(ctx, pod)
	}
}

// retryPending re-attempts every parked recreate. A success unparks the pod;
// a failure leaves it parked for the next sweep.
func (s *SupervisorHealth) retryPending(ctx context.Context) {
	s.mu.Lock()
	pods := make([]string, 0, len(s.pending))
	for pod := range s.pending {
		pods = append(pods, pod)
	}
	s.mu.Unlock()
	for _, pod := range pods {
		s.recreateOne(ctx, pod)
	}
}

// drainRecreate drains the pod's endpoints, then recreates the pod. Drain
// FIRST: while the recreate is in flight (or failed) the registry holds
// nothing for the pod, so the route leg cannot hand a task to a gateway that
// is going away. A failed DRAIN retries next sweep without a recreate (the
// expired rows are still there for the TTL leg to re-find); a failed recreate
// parks the pod in pending.
func (s *SupervisorHealth) drainRecreate(ctx context.Context, pod string) {
	if err := s.cfg.Store.DrainPod(ctx, pod); err != nil {
		s.cfg.Sink.OnLifecycle("supervisor.drain", pod, "", err)
		return
	}
	s.cfg.Sink.OnLifecycle("supervisor.drain", pod, "", nil)
	s.setPending(pod, true)
	s.recreateOne(ctx, pod)
}

// recreateOne runs one recreate attempt for a drained pod, clearing the
// pending marker on success only.
func (s *SupervisorHealth) recreateOne(ctx context.Context, pod string) {
	if err := s.cfg.Recreate.RecreatePod(ctx, pod); err != nil {
		s.cfg.Sink.OnLifecycle("supervisor.recreate", pod, "", err)
		return // stays pending — retried next sweep
	}
	s.cfg.Sink.OnLifecycle("supervisor.recreate", pod, "", nil)
	s.setPending(pod, false)
	s.resetDown(pod)
}

func (s *SupervisorHealth) isPending(pod string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[pod]
}

func (s *SupervisorHealth) setPending(pod string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.pending[pod] = true
	} else {
		delete(s.pending, pod)
	}
}

func (s *SupervisorHealth) bumpDown(pod string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downStreak[pod]++
	return s.downStreak[pod]
}

func (s *SupervisorHealth) resetDown(pod string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.downStreak, pod)
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
