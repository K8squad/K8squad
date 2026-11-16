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
	"math"
	"strconv"
	"sync"
	"time"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// Priority-ordered admission (ISI-7499): a queued run is admitted not by
// wait time alone but by its PriorityKey — work-item class first (issue /
// board work ranks above discussion / party work), then the work item's
// numeric priority, then the durable FIFO wait-start as the final,
// starvation-free tiebreak. Every component of the key is a durable Run-CR
// fact (label, annotation, condition timestamp), so a restart rebuilds the
// exact queue order: reattach jitter can never reorder runs across classes
// or priorities, and equal-key runs keep their wait-start order (ISI-7501).
// Admission is strictly NON-PREEMPTIVE (ISI-7499 D4): an admitted holder
// keeps its slot until its own terminal release or a ReclaimHolders verdict
// of dead — a later, higher-ranked arrival only queues; the gate exposes no
// path that evicts, demotes, or terminalizes a live holder.
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
// could wedge the slot indefinitely. The TTL is key-agnostic (ISI-7499): a
// silent top-ranked waiter is evicted exactly like a silent FIFO one, and it
// NEVER applies to holders.
const endpointWaiterTTL = 90 * time.Second

// Work-item admission classes (ISI-7499): a lower value admits earlier.
const (
	// ClassIssue is the class of board/issue work-item runs — the work that
	// ranks ABOVE discussion/party work in every admission decision
	// (Henrik's spec: an issue run waits for the in-flight discussion to
	// finish, but outranks every discussion run in the queue).
	ClassIssue = 0

	// ClassDiscussion is the class of discussion/party work-item runs —
	// fan-out @-mention and party-advance work that must never jump ahead
	// of queued issue work, however long it has been waiting.
	ClassDiscussion = 1
)

// UnsetPriority is the numeric priority of a run whose work item carries no
// priority stamp (discussions today; issue runs before ISI-7509 lands the
// intake stamping). Any stamped priority sorts before it; among unstamped
// runs of the same class, FIFO wait order alone decides.
const UnsetPriority = math.MaxInt32

// PriorityKey is a run's admission ordering key (ISI-7499 D2). Order is
// lexicographic: Class first (issue before discussion), then Priority with
// LOWER values admitting earlier, then WaitSince (earlier wait-start first,
// runID breaking exact ties inside the gate) as the final, starvation-free
// tiebreak. The key is derived only from durable Run-CR facts
// (runPriorityKey): the work-item-source label, the work-item-priority
// annotation, and the WaitingForEndpointSlot condition's wait-start — which
// is what makes the post-restart queue rebuild deterministic (ISI-7501's
// restart-stable FIFO, promoted to a restart-stable full ordering key).
type PriorityKey struct {
	// Class is the work-item class: ClassIssue (0) or ClassDiscussion (1).
	Class int
	// Priority is the work item's numeric priority; lower admits earlier.
	// Unstamped runs carry UnsetPriority.
	Priority int
	// WaitSince is the durable wait-start (ISI-7501): the WaitingForEndpointSlot
	// condition's LastTransitionTime once the run has queued, else the Run's
	// creationTimestamp. The zero value means "no durable fact supplied" —
	// the gate then freezes the FIFO key to its clock at first sight (the
	// legacy behavior; production always supplies a real timestamp).
	WaitSince time.Time
}

// before reports whether k admits strictly earlier than o at the class and
// priority levels (the wait-start/ID tiebreak lives in the gate, which owns
// the waiter identities).
func (k PriorityKey) before(o PriorityKey) bool {
	if k.Class != o.Class {
		return k.Class < o.Class
	}
	return k.Priority < o.Priority
}

// runPriorityKey derives a run's PriorityKey from durable Run-CR facts: the
// class from the ksquad.io/work-item-source label ("discussion" →
// ClassDiscussion, anything else — "board", unset — → ClassIssue), the
// numeric priority from the ksquad.io/work-item-priority annotation (stamped
// at intake by ISI-7509; absent or unparseable → UnsetPriority, so a
// malformed stamp can never promote a run), and the wait-start from the
// WaitingForEndpointSlot condition (LastTransitionTime) with the Run's
// creationTimestamp as the pre-queue fallback (ISI-7501). Because every
// field comes from the Run object, every lap of the same run derives an
// identical key — the property the restart rebuild relies on.
func runPriorityKey(run *api.Run) PriorityKey {
	key := PriorityKey{Class: ClassIssue, Priority: UnsetPriority}
	if run == nil {
		return key
	}
	if run.Labels[api.LabelWorkItemSource] == workItemSourceDiscussion {
		key.Class = ClassDiscussion
	}
	if v, err := strconv.Atoi(run.Annotations[api.AnnotationWorkItemPriority]); err == nil {
		key.Priority = v
	}
	key.WaitSince = run.CreationTimestamp.Time
	if c := meta.FindStatusCondition(run.Status.Conditions, ConditionEndpointSlotWait); c != nil && c.Status == metav1.ConditionTrue {
		key.WaitSince = c.LastTransitionTime.Time
	}
	return key
}

// EndpointGate bounds how many runs may concurrently hold a given BYO
// endpoint and admits waiters in (class, priority, FIFO wait-start) order
// (ISI-7499 on top of the ISI-5594/ISI-7501 FIFO base). The
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
	// tagged with a wait-start timestamp (FIFO key) and a last-seen poll time.
	// A freed slot is granted to the earliest-`since` live waiter, making
	// admission deterministic FIFO-by-wait-time instead of whichever requeue
	// timer happened to fire first (the pre-ISI-5594 starvation risk). Empty
	// sets are deleted so the map does not grow unboundedly.
	waiters map[string]map[string]*endpointWaiter
	// now is the injectable clock (time.Now in production; overridden in
	// tests to exercise TTL eviction and wait-order without real sleeps).
	now func() time.Time
	// waiterTTL is endpointWaiterTTL, overridable in tests.
	waiterTTL time.Duration
}

// endpointWaiter is one run's place in an endpoint's FIFO admission queue.
//
// The FIFO key is `since` — the moment the run began waiting on this endpoint —
// NOT an in-process counter (ISI-7501). The gate is in-process and empties on
// an operator restart, but `since` is seeded on every Acquire from a durable
// Run-CR fact (the WaitingForEndpointSlot condition's LastTransitionTime, or
// the Run's creationTimestamp before that condition stamps). So after a roll,
// re-driving waiters re-register with the SAME `since` they had before — FIFO
// order is reconstructed deterministically from the CRs rather than lost with
// the counter. runID breaks exact-timestamp ties (deterministic, not arbitrary).
type endpointWaiter struct {
	// key carries the waiter's class and priority (ISI-7499), refreshed on
	// every poll so a re-stamped priority takes effect at the next requeue.
	key PriorityKey
	// since is the durable FIFO key (ISI-7501), frozen at first sight.
	since    time.Time
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
// Fair admission (ISI-5594/ISI-7501/ISI-7499): a run that cannot be admitted
// is registered as a waiter keyed on its PriorityKey. When a slot is free it
// is granted ONLY to the (class, priority, wait-start)-minimal live waiter,
// so a freed slot is never handed to a lower-ranked run ahead of a
// higher-ranked one, nor to a later equal-key arrival ahead of a
// longer-waiting one — deterministic and starvation-free within equal keys.
// On refusal it returns one current holder's run uid (or, when the slot is
// free but a better-ranked run is ahead, that run's uid) for the wait
// message, and ok=false.
//
// Admission is strictly non-preemptive (ISI-7499 D4 invariant): this call
// can never remove, demote, or terminalize an existing holder — a
// higher-ranked arrival queues behind the current holders and is admitted
// only when a slot frees (terminal release or a dead-holder reclaim).
//
// key (ISI-7501/ISI-7499) is the restart-stable ordering key the caller
// derives from the Run CR (runPriorityKey): class, numeric priority, and
// the durable wait-start. It seeds the waiter's FIFO key so queue order
// survives an operator restart that empties the in-process gate.
func (g *EndpointGate) Acquire(endpoint, runID string, maxConcurrent int, key PriorityKey) (holder string, ok bool) {
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
	g.registerWaiter(endpoint, runID, key, now)

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

// registerWaiter records runID as a waiter on endpoint, keying it by its
// wait-start `since` and refreshing its last-seen poll time on every call (so a
// live, polling waiter keeps its place and its freshness).
//
// `since` is the restart-stable FIFO key (ISI-7501): a non-zero key.WaitSince
// is a durable Run-CR fact (condition LastTransitionTime /
// creationTimestamp), so it is re-applied on every call — stable input, no
// thrash — which is also how a re-driving waiter restores its pre-restart
// position when the gate is empty. A zero key.WaitSince (tests) freezes the
// key to the gate clock at first sight. The class/priority half of the key
// is re-applied on every call too (ISI-7499): a priority re-stamped mid-queue
// takes effect at the next poll, while the frozen `since` keeps the FIFO
// tiebreak stable.
func (g *EndpointGate) registerWaiter(endpoint, runID string, key PriorityKey, now time.Time) {
	ws := g.waiters[endpoint]
	if ws == nil {
		ws = map[string]*endpointWaiter{}
		g.waiters[endpoint] = ws
	}
	if w, ok := ws[runID]; ok {
		w.lastSeen = now
		w.key = key
		if !key.WaitSince.IsZero() {
			w.since = key.WaitSince
		}
		return
	}
	since := key.WaitSince
	if since.IsZero() {
		since = now
	}
	ws[runID] = &endpointWaiter{key: key, since: since, lastSeen: now}
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

// frontWaiter returns the (class, priority, wait-start, id)-minimal run uid
// queued on endpoint — the run a freed slot is reserved for — or "" when
// none are queued (ISI-7499 ordering on the ISI-7501 FIFO base). runID
// breaks exact-timestamp ties so the choice is deterministic across
// restarts and reconcile orderings.
func (g *EndpointGate) frontWaiter(endpoint string) string {
	front, frontW := "", (*endpointWaiter)(nil)
	for id, w := range g.waiters[endpoint] {
		if frontW == nil || w.key.before(frontW.key) ||
			(!w.key.before(frontW.key) && !frontW.key.before(w.key) &&
				(w.since.Before(frontW.since) || (w.since.Equal(frontW.since) && id < front))) {
			front, frontW = id, w
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

// EvictedHolder names one reclaimed slot: the endpoint whose slot was freed
// and the dead holder run that lost it (ISI-7483).
type EvictedHolder struct {
	Endpoint string
	RunID    string
}

// ReclaimHolders evicts every slot holder the caller reports as no longer live
// (terminal Run or Run CR gone) and returns the evicted pairs. It is the
// holder-side backstop of the ISI-5037 gate: every release site (the
// dispatcher's clean OnDone, the driver's FailEnter / cancelFinish) fires from
// the dispatch/driver paths, so a run that terminalizes through any OTHER path
// — projector force-fail, reaper force-fail, an errored follow whose OnDone
// keeps the permit by design followed by budget exhaustion — leaks its permit
// forever. The gate is the only admission authority on a single-slot endpoint,
// so ONE dead holder starves every waiter indefinitely (ISI-7483: a Failed
// run held the deepseek slot for 47+ minutes while four queued seats sat
// "exempt from zombie reap" with zero throughput). The heartbeat sweep calls
// this every tick with a liveness callback over the Run CRs. live is TRUE =
// keep the slot; a checker ERROR also counts as live — a slot is never
// reclaimed on a maybe, mirroring the reaper discipline (never reap on a
// maybe).
func (g *EndpointGate) ReclaimHolders(live func(runID string) (bool, error)) []EvictedHolder {
	g.mu.Lock()
	defer g.mu.Unlock()
	var evicted []EvictedHolder
	for endpoint, holders := range g.holders {
		for id := range holders {
			isLive, err := live(id)
			if err != nil || isLive {
				continue // fail-safe: never reclaim on a maybe
			}
			delete(holders, id)
			evicted = append(evicted, EvictedHolder{Endpoint: endpoint, RunID: id})
		}
		if len(holders) == 0 {
			delete(g.holders, endpoint)
		}
	}
	return evicted
}
