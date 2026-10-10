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

package rundrive

import (
	"errors"
	"sync"
	"time"
)

// endpoint_gate.go — the per-BYO-endpoint serialization gate (ISI-5037).
//
// Motivation: a shared BYO model endpoint (the LAN Ollama at
// http://10.0.0.185:11434, v0.32.15) serves only ONE long streaming chat
// completion at a time. Concurrent streams are accepted at TCP and queued
// server-side with an UNBOUNDED first-token wait (reproduced: the queued
// request's TTFT equals the incumbent stream's remaining duration). A second
// squad Run racing the same endpoint therefore streamed nothing within the
// shim's first-output watchdog window and died — the ~50% mid-stream run
// failures of ISI-5036. Since the endpoint cannot be made parallel from
// here, the operator serializes Runs per endpoint instead: the second Run
// waits in the drive loop (bounded requeue, legible on the CR) instead of
// dispatching into a stream that can never start in time.
//
// The gate is an in-process, leader-scoped permit map keyed by the resolved,
// normalized endpoint BaseURL (modelendpoint.Resolver already trims trailing
// slashes). In-process is sufficient: the operator is leader-elected
// single-writer (cmd/operator/main.go leader-elect) and reconcile passes are
// serialized, so the only writers are the drive loop and the dispatcher's
// follow goroutines. Correctness under restart is self-healing: the map
// starts empty and ReattachFollows re-runs buildTask for every unsettled
// lap, whose same-run Acquire re-populates the holder set. The narrow race
// (a fresh Run acquiring between leader-elect and re-attach) degrades to the
// pre-gate behavior — the loser waits or the reaper cleans up — never to a
// wrong execution.
//
// Lifecycle:
//   - Acquire: operatorDispatch.buildTask, right after the BYO endpoint
//     resolves (dispatch.go). Idempotent per run — a re-drive, retry lap, or
//     follow re-attach of the SAME run re-acquires its own permit as a no-op.
//   - Release (primary): the dispatcher's OnDone on a CLEANLY terminal
//     follow (cmd/operator/main.go) — the true "agent stopped using the
//     endpoint" moment. A follow ERROR is not proof of completion (the agent
//     may still be streaming), so an errored OnDone keeps the permit.
//   - Release (defensive): the driver's terminal FailEnter / cancelFinish
//     paths, for runs that die or are killed before (or without) a clean
//     follow — covers the acquire-then-never-dispatched and
//     follow-errored-then-budget-exhausted edges. Idempotent.

// errEndpointSlotBusy is the sentinel a buildTask wraps when its run's BYO
// endpoint is fully held by other run(s). It rides the same plumbing as
// errSandboxPending (sticky effects error → driver errors.Is branch) and
// converts to a quiet bounded requeue plus a legible CR condition — never a
// span exception, never a retry-lap burn.
var errEndpointSlotBusy = errors.New("rundrive: BYO model endpoint slot busy")

// endpointSlotWaitDelay paces the requeue of a run waiting on a busy
// endpoint. Longer than continueDelay on purpose: the holder is a full agent
// run (minutes), so a 2s poll would redo the whole buildTask read chain
// hundreds of times for nothing, while a freed slot is picked up within one
// delay — invisible against a multi-minute run.
const endpointSlotWaitDelay = 10 * time.Second

// endpointWaiterTTL bounds how long a registered-but-silent waiter keeps its
// place in the FIFO queue (ISI-5594). A live waiter re-polls every
// endpointSlotWaitDelay (10s) and refreshes its entry; the TTL is comfortably
// above that so moderate reconcile backlog never evicts (and thus reorders) a
// healthy waiter. It exists only to reclaim the queue from a waiter that
// vanished WITHOUT a terminal release (ReleaseByRun already drops the queue
// entry on every terminal/reap path) — without it a dead front-of-queue run
// could wedge the slot indefinitely under FIFO admission.
const endpointWaiterTTL = 90 * time.Second

// EndpointGate bounds how many runs may concurrently hold a given BYO
// endpoint and admits waiters in FIFO wait-order (ISI-5594). The
// zero-value-ready constructor takes the DEFAULT per-endpoint slot count
// (1 = strict serialization, the Ollama mitigation default); a per-endpoint
// override (modelendpoint maxConcurrent) is supplied on each Acquire.
type EndpointGate struct {
	mu sync.Mutex
	// defaultMax is the fallback per-endpoint slot count used when a caller
	// passes maxConcurrent <= 0 (>= 1; NewEndpointGate clamps).
	defaultMax int
	// holders maps a normalized endpoint BaseURL to the set of run uids
	// currently holding a slot on it. A run appears at most once per
	// endpoint; empty sets are deleted so the map does not grow unboundedly.
	holders map[string]map[string]struct{}
	// waiters maps an endpoint to the runs queued behind a full slot, each
	// tagged with a monotonic sequence (enqueue order) and a last-seen poll
	// time. A freed slot is granted to the lowest-seq live waiter, making
	// admission deterministic FIFO-by-wait-time instead of whichever requeue
	// timer happened to fire first (the pre-ISI-5594 starvation risk). Empty
	// sets are deleted so the map does not grow unboundedly.
	waiters map[string]map[string]*endpointWaiter
	// seq hands out strictly increasing enqueue tokens; a waiter's seq is its
	// FIFO key. uint64 never realistically wraps over a leader's lifetime.
	seq uint64
	// now is the injectable clock (time.Now in production; overridden in
	// tests to exercise TTL eviction and wait-order without real sleeps).
	now func() time.Time
	// waiterTTL is endpointWaiterTTL, overridable in tests.
	waiterTTL time.Duration
}

// endpointWaiter is one run's place in an endpoint's FIFO admission queue.
type endpointWaiter struct {
	seq      uint64
	lastSeen time.Time
}

// NewEndpointGate returns a gate whose default per-endpoint slot count is
// maxConcurrent (values < 1 clamp to 1 — strict serialization).
func NewEndpointGate(maxConcurrent int) *EndpointGate {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &EndpointGate{
		defaultMax: maxConcurrent,
		holders:    map[string]map[string]struct{}{},
		waiters:    map[string]map[string]*endpointWaiter{},
		now:        time.Now,
		waiterTTL:  endpointWaiterTTL,
	}
}

// Acquire takes a slot on endpoint for runID. maxConcurrent is the endpoint's
// own declared capacity (modelendpoint maxConcurrent); values <= 0 fall back
// to the gate's default. It is idempotent for a run that already holds a slot
// (re-drive / retry lap / follow re-attach): the same run re-acquiring its own
// permit is always granted.
//
// Fair FIFO admission (ISI-5594): a run that cannot be admitted is registered
// as a waiter keyed on its first-seen wait order. When a slot is free it is
// granted ONLY to the longest-waiting (lowest-seq) live waiter, so a freed
// slot is never handed to a late arrival ahead of a run that has been queued
// longer — deterministic, starvation-free. On refusal it returns one current
// holder's run uid (or, when the slot is free but a longer-waiting run is
// ahead, that run's uid) for the wait message, and ok=false.
func (g *EndpointGate) Acquire(endpoint, runID string, maxConcurrent int) (holder string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	max := maxConcurrent
	if max < 1 {
		max = g.defaultMax
	}

	holders := g.holders[endpoint]
	if _, mine := holders[runID]; mine {
		// Already holding: idempotent grant, and never also a waiter.
		g.dropWaiter(endpoint, runID)
		return runID, true
	}

	now := g.now()
	g.evictStaleWaiters(endpoint, now)
	g.registerWaiter(endpoint, runID, now)

	if len(holders) < max && g.frontWaiter(endpoint) == runID {
		g.dropWaiter(endpoint, runID)
		if holders == nil {
			holders = map[string]struct{}{}
			g.holders[endpoint] = holders
		}
		holders[runID] = struct{}{}
		return runID, true
	}

	// Refused: name a current holder if one exists, else the run ahead of us
	// in the FIFO queue (the slot is free but held in reserve for it).
	if h := anyKey(holders); h != "" {
		return h, false
	}
	return g.frontWaiter(endpoint), false
}

// registerWaiter records runID as a waiter on endpoint, assigning it the next
// FIFO sequence on first sight and refreshing its last-seen poll time on every
// subsequent call (so a live, polling waiter keeps its place and its freshness).
func (g *EndpointGate) registerWaiter(endpoint, runID string, now time.Time) {
	ws := g.waiters[endpoint]
	if ws == nil {
		ws = map[string]*endpointWaiter{}
		g.waiters[endpoint] = ws
	}
	if w, ok := ws[runID]; ok {
		w.lastSeen = now
		return
	}
	g.seq++
	ws[runID] = &endpointWaiter{seq: g.seq, lastSeen: now}
}

// dropWaiter removes runID from endpoint's waiter queue (on grant or release),
// pruning the endpoint's empty queue map.
func (g *EndpointGate) dropWaiter(endpoint, runID string) {
	ws := g.waiters[endpoint]
	if ws == nil {
		return
	}
	delete(ws, runID)
	if len(ws) == 0 {
		delete(g.waiters, endpoint)
	}
}

// evictStaleWaiters reclaims the queue from waiters that stopped polling past
// waiterTTL — a run that vanished without a terminal ReleaseByRun. Without it
// a dead front-of-queue waiter would hold the slot in reserve forever.
func (g *EndpointGate) evictStaleWaiters(endpoint string, now time.Time) {
	ws := g.waiters[endpoint]
	for id, w := range ws {
		if now.Sub(w.lastSeen) > g.waiterTTL {
			delete(ws, id)
		}
	}
	if len(ws) == 0 {
		delete(g.waiters, endpoint)
	}
}

// frontWaiter returns the lowest-seq (longest-waiting) run uid queued on
// endpoint, or "" when none are queued.
func (g *EndpointGate) frontWaiter(endpoint string) string {
	var front string
	var best uint64
	for id, w := range g.waiters[endpoint] {
		if front == "" || w.seq < best {
			front, best = id, w.seq
		}
	}
	return front
}

// anyKey returns one key from the set (deterministically unnecessary — the
// wait message only needs a representative holder), or "" when empty.
func anyKey(set map[string]struct{}) string {
	for k := range set {
		return k
	}
	return ""
}

// ReleaseByRun drops every slot runID holds (at most one per endpoint, any
// number of endpoints) and removes it from every endpoint's FIFO waiter queue.
// Idempotent and safe to call for a run that holds nothing — the defensive
// terminal paths call it unconditionally, which is also how a queued-then-
// terminal run yields its FIFO place to the next waiter.
func (g *EndpointGate) ReleaseByRun(runID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for endpoint, holders := range g.holders {
		if _, ok := holders[runID]; !ok {
			continue
		}
		delete(holders, runID)
		if len(holders) == 0 {
			delete(g.holders, endpoint)
		}
	}
	for endpoint := range g.waiters {
		g.dropWaiter(endpoint, runID)
	}
}
