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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/K8squad/K8squad/pkg/a2a"
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
	// busy is the gateway-local D3.3 guard #3: a process with an active
	// routed task refuses a second route. Guarded by Gateway.mu.
	busy bool
}

// gatewayRegisterInterval is the default registration/heartbeat cadence.
const gatewayRegisterInterval = 30 * time.Second

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
	if cfg.RegisterInterval <= 0 {
		cfg.RegisterInterval = gatewayRegisterInterval
	}
	if cfg.Sink == nil {
		cfg.Sink = NopSink{}
	}
	if cfg.RegisterURL != "" && (cfg.Team == "" || cfg.Role == "" || cfg.Pod == "") {
		return nil, fmt.Errorf("mesh.NewGateway: Team, Role and Pod are required when registering (RegisterURL set)")
	}
	return &Gateway{cfg: cfg}, nil
}

// Start spawns the standing pool (Min children, each pre-loaded with the
// role envelope) and, when a RegisterURL is configured, starts the
// registration/heartbeat loop. It returns only after every child spawned
// (or with the first failure) — a gateway that serves /ready before its
// pool exists would be a lying pool pod.
func (g *Gateway) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		return fmt.Errorf("mesh: gateway already started")
	}
	g.started = true
	g.mu.Unlock()

	for i := 0; i < g.cfg.Min; i++ {
		h, card, err := g.cfg.Adapter.Spawn(ctx, g.cfg.Envelope)
		if err != nil {
			return fmt.Errorf("mesh: spawn pool process %d: %w", i, err)
		}
		// Register at the seam: the adapter surfaces the card the process
		// announced (the authoritative capability contract). Persisting it
		// to coord.mesh_endpoint is the Supervisor-side half, driven by the
		// registration loop below.
		if _, err := g.cfg.Adapter.Register(ctx, h); err != nil {
			_ = g.cfg.Adapter.Stop(ctx, h)
			return fmt.Errorf("mesh: register pool process %d: %w", i, err)
		}
		g.mu.Lock()
		g.procs = append(g.procs, &gatewayProc{handle: h, card: card})
		g.mu.Unlock()
	}

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

// RegisterOnce POSTs the current endpoint set to the Supervisor ingress.
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
	for _, p := range g.Processes() {
		ep := p.Endpoint
		if advertise != "" {
			ep = advertise
		}
		reg.Processes = append(reg.Processes, RegisteredProcess{
			EndpointID: p.EndpointID,
			Endpoint:   ep,
			Card:       p.Card,
		})
	}
	if len(reg.Processes) == 0 {
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

// ProcessInfo is the read-side snapshot of one standing process.
type ProcessInfo struct {
	EndpointID string
	Endpoint   string
	Card       a2a.AgentCard
	Busy       bool
}

// Processes snapshots the pool's endpoint set (id, endpoint, card, busy).
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
		})
	}
	return out
}

// Ready reports whether the pool serves at least one standing process — the
// pool-pod readiness gate (a gateway with no children is a dead pool).
func (g *Gateway) Ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.procs) > 0
}

// Route delivers one task to a standing process and returns its live event
// stream. target selects the process by endpoint id; an empty target picks
// the FIRST idle process in spawn order (the P1.1 selection: one pool,
// prefer idle; LRU and wake-vs-overflow arrive with P1.2/P1.3 lifecycles).
//
// The D3.3 guard #3 is enforced HERE and, redundantly, by the child
// supervisor's own one-task-at-a-time contract: a busy target refuses with
// ErrProcessBusy, an all-busy pool refuses with ErrNoIdleProcess. The
// process flips back to idle when the returned stream settles (its Events
// channel closes) or is Closed. At-most-once does NOT live here — the
// a2a_task_id dedup in coord.a2a_dispatch backstops any route race (§5.4).
func (g *Gateway) Route(ctx context.Context, target string, task a2a.Task) (Stream, error) {
	g.mu.Lock()
	var pick *gatewayProc
	if target == "" {
		for _, p := range g.procs {
			if !p.busy {
				pick = p
				break
			}
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
		if pick.busy {
			g.mu.Unlock()
			return nil, ErrProcessBusy
		}
	}
	if pick == nil {
		g.mu.Unlock()
		return nil, ErrNoIdleProcess
	}
	pick.busy = true
	handle := pick.handle
	g.mu.Unlock()

	stream, err := g.cfg.Adapter.Route(ctx, handle, task)
	if err != nil {
		g.release(handle)
		return nil, err
	}
	return &gatedStream{
		inner:   stream,
		release: func() { g.release(handle) },
		events:  make(chan a2a.Event),
		quit:    make(chan struct{}),
	}, nil
}

// release flips a process back to idle (idempotent — both the stream pump's
// natural end and an explicit Close may run).
func (g *Gateway) release(h Handle) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.procs {
		if p.handle.ID() == h.ID() {
			p.busy = false
			return
		}
	}
}

// Stop terminates every standing child (the pod's graceful shutdown path).
// Each stop is best-effort: a wedged child must not block the others.
func (g *Gateway) Stop(ctx context.Context) error {
	g.mu.Lock()
	procs := append([]*gatewayProc(nil), g.procs...)
	g.mu.Unlock()
	var firstErr error
	for _, p := range procs {
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
