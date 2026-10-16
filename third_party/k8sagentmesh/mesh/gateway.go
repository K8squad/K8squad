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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// Gateway is the per-pod PID1 process-manager (chapter §2.3 D1.2): it holds a
// pod's standing role-processes as children of ONE RuntimeAdapter, tracks
// which of them is idle, and enforces the gateway-local exclusivity guard —
// one process, one active task (§5.4 D3.3 guard #3). It also owns
// registration: on boot (and on a ticker that doubles as the heartbeat) it
// POSTs its endpoint set to the Supervisor ingress, which persists them in
// coord.mesh_endpoint — the authoritative registry that survives a gateway
// restart (§2.4 D1.3).
//
// The gateway is deliberately THIN (§13.2 I2): registration, routing and
// lifecycle decisions only. No LLM traffic passes through it beyond the
// streamed task it proxies for its caller; no model policy lives here. The
// gateway holds only process handles — everything durable (custody,
// idempotency, endpoints) is in Postgres, reached through the Supervisor.
//
// This type is [generic] like the rest of the package: it speaks only the
// RuntimeAdapter seam and the A2A wire types, so it lifts to the AgentMesh
// repo at roadmap P3 unchanged.
type Gateway struct {
	cfg GatewayConfig

	mu    sync.Mutex
	procs []*gatewayProc
	// started guards Start/Stop single-use.
	started bool
	// regWG tracks the registration loop goroutine.
	regWG sync.WaitGroup
	// lifeWG tracks the lifecycle sweep loop goroutine (P1.2, D4.2: the
	// gateway owns the clock).
	lifeWG sync.WaitGroup
}

// GatewayConfig assembles a Gateway. Adapter, Envelope and Pod are required;
// the rest have sensible defaults.
type GatewayConfig struct {
	// Adapter is the runtime mechanism the gateway manages children through
	// (opencode behind the seam, chapter §1B). Required.
	Adapter RuntimeAdapter
	// Envelope is the role envelope every child is pre-loaded with — the
	// standing identity (model, skills, runtime type) the process serves
	// BEFORE any task arrives. Required (P1.1: injected at pod-spec time).
	Envelope Envelope
	// Min is the standing process count (P1.1: one team, one coder pool,
	// min:1 always-up). Values < 1 clamp to 1 — a gateway with zero
	// children is a dead pool pod, not a valid thin thread.
	Min int
	// Max is the saturation ceiling of the standing band (P1.3 preschedul-
	// ing, chapter §4 D2.2/§5.3). A pool whose every standing process is
	// busy grows standing processes toward Max (each grown slot is KEPT
	// until the idle shrink reclaims it); at Max it serves the burst with
	// transient cold-overflow processes torn down when the task settles.
	// Max <= Min (the default) keeps the P1.2 shape exactly: saturation
	// refuses with ErrNoIdleProcess and no overflow — additive-only, the
	// band activates only when a RolePool declares one (D9.3).
	Max int
	// GrowIdleShrink bounds how long a saturation-grown slot may stand
	// idle before the sweep reclaims it (the band's decay back toward
	// Min). 0 takes DefaultGrowIdleShrink; negative disables grown-slot
	// shrinking (the general HibernateToStop bound still applies).
	GrowIdleShrink time.Duration
	// Pod is the pod identity stamped on every registration (HOSTNAME
	// in-cluster). The Supervisor drains stale registrations by pod.
	Pod string
	// Team and Role are the (team, role) pool key this gateway's pool serves
	// — the dispatch lookup key (§5.2). Both required when RegisterURL is
	// set; the thin thread registers exactly one pool.
	Team string
	Role string
	// RegisterURL is the Supervisor ingress base URL registrations POST to.
	// Empty disables registration entirely (local/dev lanes) — the gateway
	// still spawns and routes, it just never becomes discoverable.
	RegisterURL string
	// AdvertiseURL is the gateway's own routable base URL
	// ("http://<podIP>:8080" in-cluster). When set, every registered
	// process advertises THIS URL as its endpoint — the Supervisor-side
	// route leg can only reach the gateway (children bind 127.0.0.1 inside
	// the pod); the ?target=<endpoint_id> query on POST /task selects the
	// process through it (chapter §5.2: gateway(target.pod).POST /task).
	// Empty keeps each handle's own endpoint (dev/local lanes where the
	// caller shares the host's network namespace).
	AdvertiseURL string
	// RegisterInterval is the registration ticker — each tick re-POSTs the
	// full endpoint set, which the Supervisor upserts and refreshes, so the
	// SAME loop is the heartbeat (§5.1 last_seen_at). Defaults to 30s,
	// comfortably inside the registry's 90s TTL at three missed beats.
	RegisterInterval time.Duration
	// Lifecycle is the per-pool lifecycle knob set (P1.2, chapter §6.2 D4.2:
	// checkInterval / idleToHibernate / hibernateToStop / alwaysUp, plus the
	// S4 Hibernate arm — ISI-5831). Zero fields take the operator defaults.
	// The gateway owns the clock — it runs the CheckInterval sweep that
	// drives IDLE→STOPPED (collapsed ladder) or IDLE→HIBERNATED→STOPPED
	// (Hibernate armed, S4) and measures the ≤3s recreate-from-envelope path.
	Lifecycle LifecycleConfig
	// Observer receives route/spawn telemetry with timing (mesh.route /
	// mesh.spawn spans + ksquad.mesh.* metrics on the k8squad side). nil
	// disables lifecycle telemetry (tests, dev lanes).
	Observer LifecycleObserver
	// HTTPClient POSTs registrations. nil uses a default client with a
	// short timeout — registration is a small JSON call, never a stream.
	HTTPClient *http.Client
	// Sink receives the gateway's own lifecycle telemetry (spawn, route,
	// register). nil uses NopSink.
	Sink TelemetrySink
}

// gatewayProc is one standing child the gateway manages.
type gatewayProc struct {
	handle Handle
	card   a2a.AgentCard
	// state is the §6 lifecycle state. busy (below) is exactly
	// state==StateActive — kept as its own flag because the P1.1 read surface
	// (ProcessInfo.Busy, /ready idle counts) reports it directly.
	state ProcState
	// idleSince is when the process last flipped to IDLE (task settled, or
	// spawn/recreate); the sweep's stop clock (D4.2).
	idleSince time.Time
	// lastUsed is when the process last took a route — the LRU tie-break
	// input for idle selection (§5.3: spread wear, keep caches warm
	// across the set).
	lastUsed time.Time
	// grown marks a saturation-grown slot (P1.3): it exists because a
	// burst demanded it, and the sweep reclaims it after GrowIdleShrink
	// idle — the band's decay back toward Min.
	grown bool
	// recreating marks a stopped slot mid-recreate so a concurrent route
	// cannot double-rebuild it (the sweep also skips it).
	recreating bool
	// busy is the gateway-local D3.3 guard #3: a process with an active
	// routed task refuses a second route. Guarded by Gateway.mu.
	busy bool
	// hibernatedSince is when the process entered HIBERNATED (S4,
	// ISI-5831) — the hibernateToStop clock measures from THIS instant,
	// not from idleSince (the frozen tier's bound is its own).
	hibernatedSince time.Time
	// hibInFlight guards the SIGSTOP landing window: the sweep has DECIDED
	// to hibernate (state already flipped) but Adapter.Hibernate has not
	// returned yet. The wake tier must not pick such a process — a SIGCONT
	// racing ahead of the SIGSTOP would stop a process mid-delivery (the
	// wake-during-write hazard class, Brainstormer-F3). Cleared once the
	// SIGSTOP lands (wake tier unlocked) or on failure (back to IDLE).
	hibInFlight bool
	// spawnedModel is the envelope Model the live process was spawned or
	// recreated with (S4 rebind-at-wake, ISI-5831): a wake compares it
	// against the pool's CURRENT envelope and respawns on drift (X5 —
	// runtimes bind model at launch, so rebind == respawn at the wake
	// boundary, never mid-stream).
	spawnedModel string
}

// gatewayRegisterInterval is the default registration/heartbeat cadence.
const gatewayRegisterInterval = 30 * time.Second

// DefaultGrowIdleShrink is the default idle bound for saturation-grown slots
// (P1.3): a slot spawned to serve a burst that has cleared is reclaimed after
// this idle duration — far shorter than HibernateToStop because the slot was
// never part of the declarative floor, and long enough that a straggler task
// in the same burst still finds the warmth it grew.
const DefaultGrowIdleShrink = 2 * time.Minute

// ErrProcessBusy is the D3.3 guard #3 refusal: the target process already
// has an active task. The HTTP edge maps it to 409 — the same shape the
// child supervisor's own one-pod-one-run guard uses — so a racing dispatcher
// sees a legible refusal, never interleaved event streams.
var ErrProcessBusy = fmt.Errorf("mesh: target process has an active task")

// ErrNoIdleProcess is the saturation signal (§5.3): every standing process
// is busy and the thin thread has no queue yet (the bounded queue and scale
// signal are P1.3 prescheduling). The dispatcher treats it as a requeue
// trigger, exactly like a busy BYO endpoint.
var ErrNoIdleProcess = fmt.Errorf("mesh: no idle process in pool")

// NewGateway validates the config and returns an unstarted gateway. Call
// Start to spawn the pool.
func NewGateway(cfg GatewayConfig) (*Gateway, error) {
	if cfg.Adapter == nil {
		return nil, fmt.Errorf("mesh.NewGateway: Adapter is required")
	}
	if cfg.Min < 1 {
		cfg.Min = 1
	}
	if cfg.Max < cfg.Min {
		cfg.Max = cfg.Min // no growth band — the P1.2 shape
	}
	if cfg.GrowIdleShrink == 0 {
		cfg.GrowIdleShrink = DefaultGrowIdleShrink
	}
	if cfg.RegisterInterval <= 0 {
		cfg.RegisterInterval = gatewayRegisterInterval
	}
	cfg.Lifecycle = cfg.Lifecycle.WithDefaults()
	if cfg.Sink == nil {
		cfg.Sink = NopSink{}
	}
	if cfg.RegisterURL != "" && (cfg.Team == "" || cfg.Role == "" || cfg.Pod == "") {
		return nil, fmt.Errorf("mesh.NewGateway: Team, Role and Pod are required when registering (RegisterURL set)")
	}
	return &Gateway{cfg: cfg}, nil
}

// Start spawns the standing pool (Min children, each pre-loaded with the
// role envelope) and starts the two gateway-owned loops: the registration
// heartbeat (when a RegisterURL is configured) and the lifecycle sweep (the
// D4.2 clock — checkInterval-paced, IDLE→STOPPED bound enforcement, alwaysUp
// exemption). It returns only after every child spawned (or with the first
// failure) — a gateway that serves /ready before its pool exists would be a
// lying pool pod.
func (g *Gateway) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		return fmt.Errorf("mesh: gateway already started")
	}
	g.started = true
	g.mu.Unlock()

	for i := 0; i < g.cfg.Min; i++ {
		h, card, err := g.spawnOne(ctx, g.cfg.Envelope, SpawnSourceCold)
		if err != nil {
			return fmt.Errorf("mesh: spawn pool process %d: %w", i, err)
		}
		g.mu.Lock()
		g.procs = append(g.procs, &gatewayProc{
			handle: h, card: card, state: StateIdle,
			idleSince: time.Now(), lastUsed: time.Now(),
			spawnedModel: g.cfg.Envelope.Model,
		})
		g.mu.Unlock()
	}

	// The lifecycle sweep runs regardless of registration — the clock is the
	// gateway's, not the Supervisor's (D4.2).
	g.lifeWG.Add(1)
	go g.lifecycleLoop(ctx)

	if g.cfg.RegisterURL != "" {
		// Register immediately, then on the heartbeat ticker. A failure is
		// reported to the sink but never fatal: the next beat retries, and
		// an unregistered gateway is simply unroutable (fail-safe — the
		// Supervisor's lookup cannot reach a pod it never heard of).
		if err := g.RegisterOnce(ctx); err != nil {
			g.cfg.Sink.OnLifecycle("gateway.register", "", "", err)
		}
		g.regWG.Add(1)
		go g.registrationLoop(ctx)
	}
	return nil
}

// spawnOne is the shared spawn leg (pool boot and recreate): launch via the
// adapter, surface the card at the seam, observe the spawn with timing.
func (g *Gateway) spawnOne(ctx context.Context, env Envelope, source SpawnSource) (Handle, a2a.AgentCard, error) {
	t0 := time.Now()
	h, card, err := g.cfg.Adapter.Spawn(ctx, env)
	eh := ""
	if h != nil {
		eh = h.ID()
	}
	g.observeSpawn(ctx, SpawnEvent{
		Team: g.cfg.Team, Role: g.cfg.Role, Pod: g.cfg.Pod, EndpointID: eh,
		Source: source, Duration: time.Since(t0), Err: err,
	})
	if err != nil {
		return nil, a2a.AgentCard{}, err
	}
	// Register at the seam: the adapter surfaces the card the process
	// announced (the authoritative capability contract). Persisting it
	// to coord.mesh_endpoint is the Supervisor-side half, driven by the
	// registration loop below.
	if _, err := g.cfg.Adapter.Register(ctx, h); err != nil {
		_ = g.cfg.Adapter.Stop(ctx, h)
		return nil, a2a.AgentCard{}, err
	}
	return h, card, nil
}

// lifecycleLoop is the D4.2 clock: on every CheckInterval tick the gateway
// sweeps its own processes and applies the §6 lifecycle bounds. The sweep
// never touches ACTIVE processes, skips mid-recreate slots, and exempts the
// whole pool when alwaysUp is set (the coordinator: pinned ACTIVE/IDLE,
// floor 1 — chapter §6 diagram note).
func (g *Gateway) lifecycleLoop(ctx context.Context) {
	defer g.lifeWG.Done()
	ticker := time.NewTicker(g.cfg.Lifecycle.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.sweep(ctx)
		}
	}
}

// sweep applies one checkInterval pass.
//
// Collapsed ladder (Hibernate disarmed — the REV-4 P1.2 shape, default):
// every IDLE process whose idle time has reached hibernateToStop is stopped —
// RAM reclaimed; the slot stays for on-demand recreate-from-envelope (§6.1:
// recreate is near-bare-exec because the image, tools and envelope are already
// staged — target ready ≤3s).
//
// Two-tier ladder (Hibernate armed — S4, ISI-5831, chapter §6.1 D4.1): an
// IDLE process past idleToHibernate is SIGSTOP'd into HIBERNATED (cheap
// freeze, instant wake — no RAM saving, stated honestly); a HIBERNATED
// process past hibernateToStop — measured from the HIBERNATE instant, not
// from idle — is woken (SIGCONT, so the graceful SIGTERM lands) and then
// terminated. alwaysUp exempts the pool from both tiers.
//
// D9.1 claim-never-frozen (ISI-5807 M4, locked f93795d5): the sweep only ever
// hibernates a VERIFIED-IDLE process — never ACTIVE, never mid-delivery — so
// no at-most-once claim can sit behind a frozen process (the lease is
// per-dispatch: acquired on dispatch, released on settle, and settle precedes
// idle by construction). The hibInFlight flag keeps a route's SIGCONT from
// racing ahead of an un-landed SIGSTOP (the wake-during-write hazard).
func (g *Gateway) sweep(ctx context.Context) {
	g.mu.Lock()
	type stopLeg struct {
		p          *gatewayProc
		h          Handle
		fromHib    bool
		endpointID string
		endpoint   string
	}
	var toStop []stopLeg
	type hibLeg struct {
		p          *gatewayProc
		h          Handle
		endpointID string
		endpoint   string
	}
	var toHibernate []hibLeg
	if !g.cfg.Lifecycle.AlwaysUp {
		now := time.Now()
		for _, p := range g.procs {
			if p.recreating || p.hibInFlight {
				continue
			}
			switch p.state {
			case StateIdle:
				if p.busy {
					continue // ACTIVE-in-flight: D9.1 — never freeze a holder
				}
				idle := now.Sub(p.idleSince)
				// P1.3 band decay: a saturation-grown slot shrinks after the
				// (short) grown-idle bound — it exists because a burst demanded
				// it, and a burst that cleared does not justify standing RAM
				// (nor a hibernate tier: grown warmth is disposable, so it
				// stops directly). Core floor slots keep the stop bounds.
				if (p.grown && g.cfg.GrowIdleShrink > 0 && idle >= g.cfg.GrowIdleShrink) ||
					idle >= g.cfg.Lifecycle.HibernateToStop {
					// Flip FIRST, terminate after: once marked Stopped the slot
					// is unpickable by Route, so a task can never land on a
					// process being torn down. recreating keeps the in-flight
					// teardown slot out of Route's recreate picks until the old
					// process is fully gone.
					p.state = StateStopped
					p.recreating = true
					toStop = append(toStop, stopLeg{p: p, h: p.handle, endpointID: p.handle.ID(), endpoint: p.handle.Endpoint()})
					continue
				}
				if g.cfg.Lifecycle.Hibernate && idle >= g.cfg.Lifecycle.IdleToHibernate {
					// Same flip-first discipline: the slot leaves IDLE under
					// the mutex, and hibInFlight keeps the wake tier away
					// until the SIGSTOP lands.
					p.state = StateHibernated
					p.hibernatedSince = now
					p.hibInFlight = true
					toHibernate = append(toHibernate, hibLeg{p: p, h: p.handle, endpointID: p.handle.ID(), endpoint: p.handle.Endpoint()})
				}
			case StateHibernated:
				// busy is impossible here in practice (Route flips a picked
				// hibernated process to ACTIVE under the same mutex), but the
				// guard keeps the invariant local and cheap.
				if p.busy {
					continue
				}
				if now.Sub(p.hibernatedSince) >= g.cfg.Lifecycle.HibernateToStop {
					p.state = StateStopped
					p.recreating = true
					toStop = append(toStop, stopLeg{p: p, h: p.handle, fromHib: true, endpointID: p.handle.ID(), endpoint: p.handle.Endpoint()})
				}
			}
		}
	}
	g.mu.Unlock()

	for _, leg := range toHibernate {
		err := g.cfg.Adapter.Hibernate(ctx, leg.h)
		g.mu.Lock()
		leg.p.hibInFlight = false
		if err != nil {
			// The SIGSTOP did not land — the process is still running, so the
			// slot is IDLE again (idleSince kept: the bound still holds, the
			// next sweep retries) and routable by the idle tier.
			leg.p.state = StateIdle
		}
		g.mu.Unlock()
		g.cfg.Sink.OnLifecycle("lifecycle.hibernate", leg.endpointID, leg.endpoint, err)
	}

	for _, leg := range toStop {
		g.cfg.Sink.OnLifecycle("lifecycle.stop", leg.endpointID, leg.endpoint, nil)
		if leg.fromHib {
			// Wake first so the graceful SIGTERM in Stop is handled promptly —
			// a SIGSTOP'd process holds signals pending until SIGCONT, and
			// would otherwise ride the grace period straight into SIGKILL. A
			// failed wake is not fatal here: Close's SIGKILL terminates a
			// frozen process regardless.
			if werr := g.cfg.Adapter.Wake(ctx, leg.h); werr != nil {
				g.cfg.Sink.OnLifecycle("lifecycle.wake", leg.endpointID, leg.endpoint, werr)
			}
		}
		if err := g.cfg.Adapter.Stop(ctx, leg.h); err != nil {
			// Report, do not revert: a wedged process is exactly what
			// Recreate's best-effort close exists to clean up after. The
			// stale handle is closed again on recreate.
			g.cfg.Sink.OnLifecycle("lifecycle.stop", leg.endpointID, leg.endpoint, err)
		}
		g.mu.Lock()
		leg.p.recreating = false // the slot is now a clean recreate candidate
		g.mu.Unlock()
	}
	if len(toStop) > 0 {
		// Re-register immediately so the ingress prunes the stopped
		// endpoints from coord.mesh_endpoint right away instead of waiting
		// out the registry TTL (the registration POST carries only standing
		// processes; the ingress PrunePod drops the rest). Hibernated
		// processes stay standing and registered — routable via the wake
		// tier (§5.3: prefer idle, then hibernated).
		if err := g.RegisterOnce(ctx); err != nil {
			g.cfg.Sink.OnLifecycle("gateway.register", "", "", err)
		}
	}
}

// registrationLoop re-POSTs the endpoint set on the heartbeat cadence.
func (g *Gateway) registrationLoop(ctx context.Context) {
	defer g.regWG.Done()
	ticker := time.NewTicker(g.cfg.RegisterInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.RegisterOnce(ctx); err != nil {
				g.cfg.Sink.OnLifecycle("gateway.register", "", "", err)
			}
		}
	}
}

// Registration is the wire payload a gateway POSTs to the Supervisor
// ingress: the pod identity, the (team, role) pool key, and the live
// endpoint set. One POST is both the initial registration and the heartbeat.
type Registration struct {
	Pod       string
	Team      string
	Role      string
	Processes []RegisteredProcess
}

// RegisteredProcess is one standing process in a Registration.
type RegisteredProcess struct {
	EndpointID string
	Endpoint   string
	Card       a2a.AgentCard
}

// RegisterOnce POSTs the current standing endpoint set to the Supervisor
// ingress. Stopped slots are excluded — the ingress's PrunePod drops every
// endpoint of this pod that is not in the POST, so the stopped processes
// leave coord.mesh_endpoint promptly rather than at TTL expiry.
//
// A gateway whose every slot is STOPPED legitimately POSTs an EMPTY set (the
// scale-to-zero end state — the prune is the whole point). Refusing an empty
// POST applies only to a gateway that never held any process, which is the
// broken-boot case the P1.1 guard existed for.
func (g *Gateway) RegisterOnce(ctx context.Context) error {
	client := g.cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	reg := Registration{Pod: g.cfg.Pod, Team: g.cfg.Team, Role: g.cfg.Role}
	// The advertised endpoint is the gateway's own URL when configured: the
	// operator-side route leg reaches the POD, and ?target=<endpoint_id>
	// picks the process — a child's 127.0.0.1:<port> is routable only
	// inside the pod (§5.2).
	advertise := strings.TrimRight(g.cfg.AdvertiseURL, "/")
	// heldAny: the gateway HAS pool slots (standing or stopped). Only a
	// gateway that never spawned anything refuses the empty POST — a pool
	// whose every slot legitimately stopped must be able to prune. The
	// process set is snapshotted UNDER the lock: a concurrent Route may be
	// recreating a slot (replacing its handle/card) — reading those fields
	// outside the mutex races the swap.
	g.mu.Lock()
	heldAny := len(g.procs) > 0
	g.mu.Unlock()
	for _, p := range g.standing() {
		ep := p.handle.Endpoint()
		if advertise != "" {
			ep = advertise
		}
		reg.Processes = append(reg.Processes, RegisteredProcess{
			EndpointID: p.handle.ID(),
			Endpoint:   ep,
			Card:       p.card,
		})
	}
	if len(reg.Processes) == 0 && !heldAny {
		return fmt.Errorf("mesh: refusing to register an empty endpoint set")
	}
	payload, err := json.Marshal(reg)
	if err != nil {
		return fmt.Errorf("mesh: marshal registration: %w", err)
	}
	url := g.cfg.RegisterURL + "/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("mesh: build register POST: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mesh: register POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mesh: register POST %s answered %s", url, resp.Status)
	}
	g.cfg.Sink.OnLifecycle("gateway.register", "", "", nil)
	return nil
}

// standingProc is the registration read's per-slot snapshot: handle and card
// copied out under g.mu because the recreate path swaps those same slot
// fields under the mutex (recreateSlot) — returning *gatewayProc pointers
// would leave the registration loop racing the swap (caught by -race in CI:
// RegisterOnce read slot.handle while recreateSlot wrote it).
type standingProc struct {
	handle Handle
	card   a2a.AgentCard
}

// standing snapshots the slots that currently hold a live process (everything
// but STOPPED — the recreate-on-demand slots).
func (g *Gateway) standing() []standingProc {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]standingProc, 0, len(g.procs))
	for _, p := range g.procs {
		if p.state != StateStopped {
			out = append(out, standingProc{handle: p.handle, card: p.card})
		}
	}
	return out
}

// ProcessInfo is the read-side snapshot of one pool slot.
type ProcessInfo struct {
	EndpointID string
	Endpoint   string
	Card       a2a.AgentCard
	Busy       bool
	// State is the §6 lifecycle state (active|idle|hibernated|stopped). A
	// stopped slot keeps its last EndpointID/Endpoint for debugging; its
	// handle is stale and it is excluded from registration.
	State ProcState
	// Grown marks a saturation-grown slot (P1.3) — standing beyond the
	// declarative floor, reclaimed after GrowIdleShrink idle.
	Grown bool
	// Recreating reports whether the slot is currently being rebuilt via
	// Recreate (route-driven) or torn down via Stop (sweep-driven). A stopped
	// slot with Recreating=true is in-flight and not yet ready for a new route.
	Recreating bool
}

// Processes snapshots the pool's slot set (id, endpoint, card, busy, state).
func (g *Gateway) Processes() []ProcessInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]ProcessInfo, 0, len(g.procs))
	for _, p := range g.procs {
		out = append(out, ProcessInfo{
			EndpointID: p.handle.ID(),
			Endpoint:   p.handle.Endpoint(),
			Card:       p.card,
			Busy:       p.busy,
			State:      p.state,
			Grown:      p.grown,
			Recreating: p.recreating,
		})
	}
	return out
}

// Ready reports whether the pool can serve a route: at least one standing
// process OR at least one stopped slot (recreate-on-demand is serving
// capability — §6.1: the next A2A request rebuilds from the envelope in
// near-bare-exec time). A pool pod must not flip NotReady just because it
// legitimately scaled its processes to zero overnight; that would cycle the
// pod and defeat the reclaim.
func (g *Gateway) Ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.procs) > 0
}

// ProbeReady reports whether the gateway serving baseURL answers GET /ready
// with 200 inside ctx — the Supervisor-side "gateway up" signal (P1.4,
// D6.1/D6.2). /ready (not /health) is the probe surface deliberately: the
// health loop and the cphealth dep care about SERVING capability (the pool
// can take a route), and a wedged-but-alive gateway must read as down so the
// supervisor drains and recreates it instead of trusting a bare
// process-alive ping. A nil client uses a default with a short timeout — the
// probe always runs under a caller-bounded ctx (the cphealth probe timeout /
// the supervisor sweep).
func ProbeReady(ctx context.Context, client *http.Client, baseURL string) bool {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/ready", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Route delivers one task to a standing process and returns its live event
// stream. target selects the process by endpoint id; an empty target picks
// the LEAST-RECENTLY-USED idle process (§5.3 LRU tie-break), and — the P1.2
// lifecycle — when no process stands idle, recreates a STOPPED slot from the
// envelope on demand (selection=overflow, §6.1: near-bare-exec, target ≤3s).
// With a growth band (Max > Min, P1.3) a fully-busy pool grows a new standing
// slot toward Max or serves a transient cold-overflow process at Max (§5.3).
// A target naming a stopped slot recreates that slot.
//
// The D3.3 guard #3 is enforced HERE and, redundantly, by the child
// supervisor's own one-task-at-a-time contract: a busy target refuses with
// ErrProcessBusy, an all-busy pool with no recreate slot refuses with
// ErrNoIdleProcess. The process flips back to idle when the returned stream
// settles (its Events channel closes) or is Closed. At-most-once does NOT
// live here — the a2a_task_id dedup in coord.a2a_dispatch backstops any
// route race (§5.4).
func (g *Gateway) Route(ctx context.Context, target string, task a2a.Task) (Stream, error) {
	arrival := time.Now()
	g.mu.Lock()
	var pick *gatewayProc
	selection := SelectionIdle
	if target == "" {
		// Idle tier with the §5.3 LRU tie-break: among idle standing
		// processes pick the least-recently-used — the warm investment is
		// spread across the set instead of hammering spawn order (which
		// would leave the tail cold and the head cache-thrashed).
		for _, p := range g.procs {
			if p.recreating || p.busy || p.state != StateIdle {
				continue
			}
			if pick == nil || p.lastUsed.Before(pick.lastUsed) {
				pick = p
			}
		}
		if pick == nil && g.cfg.Lifecycle.Hibernate {
			// S4 wake tier (§5.3: prefer idle, then hibernated — §6.1 D4.1):
			// a HIBERNATED process is woken with SIGCONT on the route — the
			// instant-wake path hibernation exists for. hibInFlight slots
			// are skipped: their SIGSTOP has not landed, and a SIGCONT
			// racing ahead of it would freeze a process mid-delivery (the
			// wake-during-write hazard, Brainstormer-F3).
			for _, p := range g.procs {
				if p.recreating || p.hibInFlight || p.state != StateHibernated {
					continue
				}
				if pick == nil || p.lastUsed.Before(pick.lastUsed) {
					pick = p
				}
			}
			if pick != nil {
				selection = SelectionWake
			}
		}
		if pick == nil {
			// No idle standing process: the RAM-reclaim trade pays off here —
			// rebuild a stopped slot from the pre-loaded envelope on demand.
			for _, p := range g.procs {
				if p.state == StateStopped && !p.recreating {
					p.recreating = true
					pick = p
					selection = SelectionOverflow
					break
				}
			}
		}
		if pick == nil && g.cfg.Max > g.cfg.Min {
			// P1.3 saturation tiers (§5.3): grow standing toward Max, then
			// transient cold-overflow at Max. Band-less pools (Max==Min)
			// keep the P1.2 refusal exactly.
			g.mu.Unlock()
			return g.routeSaturated(ctx, task, arrival)
		}
	} else {
		for _, p := range g.procs {
			if p.handle.ID() == target {
				pick = p
				break
			}
		}
		if pick == nil {
			g.mu.Unlock()
			return nil, fmt.Errorf("mesh: no such target process %q", target)
		}
		if pick.state == StateStopped && !pick.recreating {
			pick.recreating = true
			selection = SelectionOverflow
		}
		if pick.state == StateHibernated && !pick.hibInFlight && !pick.recreating {
			selection = SelectionWake
		}
		// A target still inside its SIGSTOP landing window is a transient
		// race: refuse legibly (409) so the re-drive retries once the
		// hibernate settles one way or the other.
		if pick.state == StateHibernated && pick.hibInFlight {
			g.mu.Unlock()
			return nil, ErrProcessBusy
		}
	}
	if pick == nil {
		g.mu.Unlock()
		return nil, ErrNoIdleProcess
	}
	if pick.busy || pick.recreating && selection != SelectionOverflow {
		g.mu.Unlock()
		return nil, ErrProcessBusy
	}
	pick.busy = true
	pick.state = StateActive
	pick.lastUsed = time.Now()
	handle := pick.handle
	needRecreate := selection == SelectionOverflow
	// S4 rebind-at-wake (X5, ISI-5831): runtimes bind model at launch, so a
	// wake whose envelope model DRIFTED since the process was spawned is a
	// respawn from the CURRENT envelope — never a mid-stream rebind, and
	// never a stale-model SIGCONT either.
	rebindWake := false
	if selection == SelectionWake && pick.spawnedModel != g.cfg.Envelope.Model {
		rebindWake = true
		needRecreate = true
		pick.recreating = true
	}
	slot := pick
	g.mu.Unlock()

	if needRecreate {
		var err error
		handle, err = g.recreateSlot(ctx, slot)
		g.mu.Lock()
		slot.recreating = false
		g.mu.Unlock()
		if err != nil {
			// The slot stays STOPPED and retryable; the busy flag set above
			// must not leak a phantom ACTIVE.
			g.mu.Lock()
			slot.busy = false
			if slot.state == StateActive {
				slot.state = StateStopped
			}
			g.mu.Unlock()
			return nil, err
		}
		// The recreated process takes this task immediately: it is ACTIVE
		// until the stream settles and release() flips it back to IDLE.
		g.mu.Lock()
		slot.state = StateActive
		g.mu.Unlock()
		if rebindWake {
			// The wake took the rebind path — observe it with the respawn's
			// duration (the honest cost of a model-moved wake; the spawn
			// itself is already observed by recreateSlot as source=envelope).
			g.observeWake(ctx, WakeEvent{
				Team: g.cfg.Team, Role: g.cfg.Role, Pod: g.cfg.Pod,
				EndpointID: handle.ID(), Rebind: true,
			})
		}
	} else if selection == SelectionWake {
		// SIGCONT before delivery: the task must land on a RUNNING process.
		// A failed wake (stale handle — the process died frozen) reverts the
		// slot to HIBERNATED so the next sweep's hibernateToStop bound (or a
		// later wake retry) heals it; the route is refused loudly and the
		// re-drive retries through the full selection ladder.
		t0 := time.Now()
		werr := g.cfg.Adapter.Wake(ctx, handle)
		if werr == nil {
			g.observeWake(ctx, WakeEvent{
				Team: g.cfg.Team, Role: g.cfg.Role, Pod: g.cfg.Pod,
				EndpointID: handle.ID(), Duration: time.Since(t0),
			})
		} else {
			g.observeWake(ctx, WakeEvent{
				Team: g.cfg.Team, Role: g.cfg.Role, Pod: g.cfg.Pod,
				EndpointID: handle.ID(), Duration: time.Since(t0), Err: werr,
			})
			g.mu.Lock()
			slot.busy = false
			if slot.state == StateActive {
				slot.state = StateHibernated
			}
			g.mu.Unlock()
			g.observeRoute(ctx, RouteEvent{
				Team: g.cfg.Team, Role: g.cfg.Role, Pod: g.cfg.Pod,
				EndpointID: handle.ID(), Selection: selection,
				Duration: time.Since(arrival), Err: werr,
			})
			return nil, werr
		}
	}

	stream, err := g.cfg.Adapter.Route(ctx, handle, task)
	if err != nil {
		g.release(handle)
		// A failed delivery still observes the route (outcome=error): the
		// ready-latency histogram must see delivery failures, not just
		// successes — a recreate that never delivers is the SLO breach.
		// Guard-refusals (busy/saturated/no-target) do NOT span: they never
		// reached a process and would pollute the SLO window.
		g.observeRoute(ctx, RouteEvent{
			Team: g.cfg.Team, Role: g.cfg.Role, Pod: g.cfg.Pod,
			EndpointID: handle.ID(), Selection: selection,
			Duration: time.Since(arrival), Err: err,
		})
		return nil, err
	}
	g.observeRoute(ctx, RouteEvent{
		Team: g.cfg.Team, Role: g.cfg.Role, Pod: g.cfg.Pod,
		EndpointID: handle.ID(), Selection: selection,
		Duration: time.Since(arrival), Err: nil,
	})
	return &gatedStream{
		inner:   stream,
		release: func() { g.release(handle) },
		events:  make(chan a2a.Event),
		quit:    make(chan struct{}),
	}, nil
}

// routeSaturated is the P1.3 §5.3 saturation ladder, entered only when a
// band is configured (Max > Min) and the route found no idle standing
// process and no stopped slot to recreate. Tiers, in order:
//
//  1. GROW — standing slots < Max: spawn a NEW standing slot from the
//     envelope (spawn source=envelope — image/tools are already staged in
//     the pod, so this is near-bare-exec like recreate). The slot is KEPT:
//     this is the pool's standing count growing toward max under load, and
//     GrowIdleShrink reclaims it once the burst has cleared.
//  2. TRANSIENT cold-overflow at Max: serve the task with a process that
//     is NOT a pool slot — torn down when the task settles (burst-clear
//     reclaim, never kept as warmth).
//
// The pool_saturated signal (chapter §8.1) fires on entry — with in-pod
// growth the reaction is immediate (tier 1/2); the event records the
// pressure for the Supervisor's status/telemetry surfaces (the full NATS
// mesh-entity ride is P1.4, ISI-5823).
func (g *Gateway) routeSaturated(ctx context.Context, task a2a.Task, arrival time.Time) (Stream, error) {
	g.cfg.Sink.OnLifecycle("pool.saturated", "", "", nil)

	// Snapshot the envelope under the lock: UpdateEnvelope (the S4 rebind
	// drift source) may move it concurrently with a saturation spawn.
	g.mu.Lock()
	env := g.cfg.Envelope
	g.mu.Unlock()
	h, card, err := g.spawnOne(ctx, env, SpawnSourceEnvelope)
	if err != nil {
		g.cfg.Sink.OnLifecycle("pool.grow", "", "", err)
		return nil, fmt.Errorf("mesh: saturation spawn: %w", err)
	}

	// Reserve atomically: a concurrent saturated route may have taken the
	// last grow room while this spawn ran — then this process serves as
	// transient overflow instead of a kept slot (max stays honest).
	g.mu.Lock()
	grown := false
	if len(g.procs) < g.cfg.Max {
		grown = true
		g.procs = append(g.procs, &gatewayProc{
			handle: h, card: card, state: StateActive,
			idleSince: time.Now(), lastUsed: time.Now(), grown: true, busy: true,
			spawnedModel: g.cfg.Envelope.Model,
		})
	}
	g.mu.Unlock()

	if !grown {
		// Tier 2 — transient cold-overflow: not a slot; dies with the task.
		stream, rerr := g.cfg.Adapter.Route(ctx, h, task)
		g.observeRoute(ctx, RouteEvent{
			Team: g.cfg.Team, Role: g.cfg.Role, Pod: g.cfg.Pod,
			EndpointID: h.ID(), Selection: SelectionOverflow,
			Duration: time.Since(arrival), Err: rerr,
		})
		if rerr != nil {
			_ = g.cfg.Adapter.Stop(context.WithoutCancel(ctx), h)
			return nil, rerr
		}
		return &gatedStream{
			inner: stream,
			// Detached context: the overflow process must die when the burst
			// clears even if the request context is already gone.
			release: func() { _ = g.cfg.Adapter.Stop(context.WithoutCancel(ctx), h) },
			events:  make(chan a2a.Event),
			quit:    make(chan struct{}),
		}, nil
	}

	// Tier 1 — grow: the new process joins the standing set, ACTIVE for
	// this task, idle (and shrink-eligible) after it settles.
	stream, rerr := g.cfg.Adapter.Route(ctx, h, task)
	g.observeRoute(ctx, RouteEvent{
		Team: g.cfg.Team, Role: g.cfg.Role, Pod: g.cfg.Pod,
		EndpointID: h.ID(), Selection: SelectionOverflow,
		Duration: time.Since(arrival), Err: rerr,
	})
	if rerr != nil {
		// Delivery failed but the slot stands as idle grown warmth — the
		// next route finds it instead of spawning again.
		g.release(h)
		return nil, rerr
	}
	return &gatedStream{
		inner:   stream,
		release: func() { g.release(h) },
		events:  make(chan a2a.Event),
		quit:    make(chan struct{}),
	}, nil
}

// recreateSlot rebuilds a stopped slot from the envelope via the adapter's
// Recreate op (§6.1: best-effort close of the stale process — it may already
// be dead, which is the common trigger — then a near-bare-exec spawn into the
// staged image/tools). The returned handle is the slot's new process.
func (g *Gateway) recreateSlot(ctx context.Context, slot *gatewayProc) (Handle, error) {
	// Snapshot the envelope under the lock (UpdateEnvelope may move it).
	g.mu.Lock()
	env := g.cfg.Envelope
	g.mu.Unlock()
	t0 := time.Now()
	nh, err := g.cfg.Adapter.Recreate(ctx, slot.handle, env)
	eh := ""
	if nh != nil {
		eh = nh.ID()
	}
	g.observeSpawn(ctx, SpawnEvent{
		Team: g.cfg.Team, Role: g.cfg.Role, Pod: g.cfg.Pod, EndpointID: eh,
		Source: SpawnSourceEnvelope, Duration: time.Since(t0), Err: err,
	})
	if err != nil {
		return nil, fmt.Errorf("mesh: recreate from envelope: %w", err)
	}
	// Register at the seam returns the card the recreated process serves —
	// the authoritative capability contract, exactly like the boot path.
	card, err := g.cfg.Adapter.Register(ctx, nh)
	if err != nil {
		_ = g.cfg.Adapter.Stop(ctx, nh)
		return nil, fmt.Errorf("mesh: register recreated process: %w", err)
	}
	g.mu.Lock()
	slot.handle = nh
	slot.card = card
	slot.state = StateIdle
	slot.idleSince = time.Now()
	slot.spawnedModel = env.Model
	g.mu.Unlock()
	return nh, nil
}

// observeRoute reports the routing decision to the observer (no-op without
// one). Called with the dispatch request's ctx so the observer's span nests
// under the run trace (§8.2).
func (g *Gateway) observeRoute(ctx context.Context, e RouteEvent) {
	if g.cfg.Observer != nil {
		g.cfg.Observer.ObserveRoute(ctx, e)
	}
}

// observeSpawn reports a spawn/recreate to the observer (no-op without one).
func (g *Gateway) observeSpawn(ctx context.Context, e SpawnEvent) {
	if g.cfg.Observer != nil {
		g.cfg.Observer.ObserveSpawn(ctx, e)
	}
}

// observeWake reports a hibernation wake to the observer (no-op without one).
func (g *Gateway) observeWake(ctx context.Context, e WakeEvent) {
	if g.cfg.Observer != nil {
		g.cfg.Observer.ObserveWake(ctx, e)
	}
}

// UpdateEnvelope moves the pool's role envelope (S4 rebind-at-wake,
// ISI-5831): the drift source a supervisor signal (a role model rebalance)
// writes, and the wake tier's rebind comparison reads. The new envelope
// takes effect at the NEXT wake boundary — running processes are NEVER
// rebound mid-stream (X5); each is respawned from the current envelope the
// next time it is woken (or recreated, which always rebuilds from it).
func (g *Gateway) UpdateEnvelope(env Envelope) {
	g.mu.Lock()
	g.cfg.Envelope = env
	g.mu.Unlock()
}

// release flips a process back to idle (idempotent — both the stream pump's
// natural end and an explicit Close may run). The idle clock restarts here:
// this instant is what the sweep's stop bound measures from (D4.2).
func (g *Gateway) release(h Handle) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.procs {
		if p.handle.ID() == h.ID() {
			p.busy = false
			if p.state == StateActive {
				p.state = StateIdle
				p.idleSince = time.Now()
			}
			return
		}
	}
}

// Stop terminates every standing child (the pod's graceful shutdown path).
// Each stop is best-effort: a wedged child must not block the others. Slots
// already STOPPED by the lifecycle sweep are skipped — their process is gone.
// A HIBERNATED child is woken first so its SIGTERM is handled promptly
// instead of riding the grace period into SIGKILL.
func (g *Gateway) Stop(ctx context.Context) error {
	g.mu.Lock()
	procs := append([]*gatewayProc(nil), g.procs...)
	g.mu.Unlock()
	var firstErr error
	for _, p := range procs {
		if p.state == StateStopped {
			continue
		}
		if p.state == StateHibernated {
			if err := g.cfg.Adapter.Wake(ctx, p.handle); err != nil && firstErr == nil {
				// Non-fatal: Close's SIGKILL terminates a frozen process.
				firstErr = err
			}
		}
		if err := g.cfg.Adapter.Stop(ctx, p.handle); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// gatedStream wraps a routed task's Stream so the process's busy flag
// releases exactly once — when the inner stream settles (Events closes) OR
// when the follower drops it (Close), whichever comes first. The event
// forwarding adds one goroutine per routed task; the price of keeping the
// busy flag honest.
type gatedStream struct {
	inner   Stream
	release func()
	events  chan a2a.Event
	quit    chan struct{}

	pumpOnce    sync.Once
	closeOnce   sync.Once
	releaseOnce sync.Once
}

func (s *gatedStream) doRelease() { s.releaseOnce.Do(s.release) }

func (s *gatedStream) pump() {
	defer close(s.events)
	defer s.doRelease()
	// Hoist the inner channel ONCE: Events() is a per-stream channel closed
	// exactly once, but a Stream is free to hand out a fresh channel per
	// call — re-calling it per iteration would never reach the close.
	events := s.inner.Events()
	for {
		ev, ok := <-events
		if !ok {
			return
		}
		select {
		case s.events <- ev:
		case <-s.quit:
			return
		}
	}
}

// Events returns the forwarded event channel; it closes when the inner
// stream settles.
func (s *gatedStream) Events() <-chan a2a.Event {
	s.pumpOnce.Do(func() { go s.pump() })
	return s.events
}

// Status snapshots the inner stream's status.
func (s *gatedStream) Status() a2a.Status { return s.inner.Status() }

// Close tears the inner stream down and releases the process. Idempotent.
func (s *gatedStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.quit)
		_ = s.inner.Close()
		s.doRelease()
	})
	return nil
}

var _ Stream = (*gatedStream)(nil)
