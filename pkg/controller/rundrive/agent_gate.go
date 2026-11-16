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
	"context"
	"errors"
	"sync"
	"time"

	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// agent_gate.go — the per-agent non-preemptive dispatch queue (ISI-7499 C4,
// plan D3/D4). An agent works ONE run at a time: a second ready Run for the
// same (team, agent) waits in a per-agent priority queue and is admitted only
// when the in-flight run goes terminal — a higher-priority arrival NEVER
// cancels or evicts the incumbent (Henrik's non-preemption rule: "never STOP
// an agent while it is working"). Together with the EndpointGate (ISI-5037,
// the per-ENDPOINT queue) this closes the party-mode thrash: contention on
// either axis (one agent, one endpoint slot) degrades to an ordered queue
// with a legible wait marker, never to a terminal failure.
//
// Admission is priority-ordered (plan D1): class first (board/issue runs
// outrank discussion/party runs), then the item's numeric priority, then FIFO
// wait order, then runID as the deterministic tiebreak. The class is derived
// TODAY from the Run's api.LabelWorkItemSource label (board vs discussion,
// stamped end-to-end by intake since ADR-0024c); the numeric priority field
// is the seam C2 (ISI-7509) fills when it carries the board priority onto the
// Run — until then every class member ties at 0 and order is pure FIFO, the
// exact pre-C4 behavior.
//
// Like the EndpointGate, the gate is in-process and leader-scoped (the
// operator is leader-elected single-writer). Correctness under restart is the
// same self-healing shape: the map starts empty and ReattachFollows re-runs
// buildTask for every unsettled lap, whose same-run Acquire re-grants the
// in-flight slot idempotently; waiters re-register on their next 10s re-poll
// with the SAME restart-stable wait key (the WaitingForAgentSlot condition's
// LastTransitionTime, else the Run creationTimestamp — the ISI-7501 durable
// FIFO-key pattern), so queue order is reconstructed from the CRs.
//
// Lifecycle:
//   - Acquire (primary): the driver's pre-dispatch admission at the
//     claiming/claiming_sandbox edge, BEFORE the machine binds a sandbox — a
//     refused run parks pod-less at the wait cadence with the legible
//     WaitingForAgentSlot condition (no lap consumed, no retry budget burnt).
//   - Acquire (idempotent re-grant): operatorDispatch.buildTask, so a re-drive
//     lap, retry lap, or follow re-attach of the SAME run re-grants its own
//     slot as a no-op — the sticky-admission half of D4: once a run is
//     admitted, no later reconcile lap or higher-priority arrival can demote
//     it; only a terminal release (or the sweep's dead-holder reclaim) frees
//     the slot.
//   - Release: the driver's terminal paths (the IsTerminal absorb, the
//     FailEnter branch of retryOrFail, cancelFinish) — "admitted only when
//     the current run goes terminal" is the whole exit contract.
//   - ReclaimInflight: the heartbeat sweep's backstop, mirroring the
//     EndpointGate's ReclaimHolders (ISI-7483) — a holder that died through
//     any path the release sites missed is evicted by the sweep's liveness
//     callback; never reclaimed on a maybe.

// errAgentSlotBusy is the sentinel a buildTask (or the driver's pre-dispatch
// admission) wraps when the run's agent already has another run in flight. It
// rides the same plumbing as errEndpointSlotBusy: the driver converts it to a
// quiet bounded requeue plus the legible WaitingForAgentSlot CR condition —
// never a span exception, never a retry-lap burn, never terminal.
var errAgentSlotBusy = errors.New("rundrive: agent already has a run in flight")

// agentSlotWaitDelay paces the requeue of a run waiting on its busy agent —
// the same cadence as endpointSlotWaitDelay (the holder is a full agent run,
// minutes; the waiter re-polls and refreshes its queue entry each tick).
const agentSlotWaitDelay = 10 * time.Second

// agentWaiterTTL bounds how long a registered-but-silent waiter keeps its
// queue place (endpointWaiterTTL's discipline: comfortably above the 10s
// re-poll, so only a waiter that vanished without a release is evicted).
const agentWaiterTTL = 90 * time.Second

// dispatchClass is the run's priority CLASS (plan D1): board/issue work (0)
// outranks discussion/party work (1). Smaller is more important.
type dispatchClass int

const (
	// DispatchClassBoard is an issue/ticket run — the class that outranks
	// everything else in every queue (Henrik's rule 1).
	DispatchClassBoard dispatchClass = 0
	// DispatchClassDiscussion is a party-advance / @-mention run minted from
	// the discussion room.
	DispatchClassDiscussion dispatchClass = 1
)

// dispatchClassForRun derives the run's priority class from its
// api.LabelWorkItemSource label — the durable origin marker intake stamps on
// every minted Run (coord.work_item.source, ADR-0024c). Anything other than
// the discussion marker (including a missing label: legacy runs, hand-minted
// Runs, a label read that predates the stamp) is the board default —
// fail-closed toward the HIGHER-priority class, never the lower.
func dispatchClassForRun(run *api.Run) dispatchClass {
	if run != nil && run.Labels[api.LabelWorkItemSource] == workItemSourceDiscussion {
		return DispatchClassDiscussion
	}
	return DispatchClassBoard
}

// DispatchPriorityKey is the total order over queued runs (plan D1):
// {class, priority, since, runID}. Class first (board < discussion), then the
// item's numeric priority (smaller = more important; 0 = unset, the pre-C2
// default), then FIFO wait start (restart-stable), then runID as the
// deterministic tiebreak. This is the same composite the EndpointGate's
// waiters move to in C3 (ISI-7510) — one key shape shared by every dispatch
// queue, so "issue > discussion, then priority, then FIFO" reads identically
// on the endpoint axis and the agent axis.
type DispatchPriorityKey struct {
	Class    dispatchClass
	Priority int
	Since    time.Time
	RunID    string
}

// before reports whether k strictly outranks o in the queue order.
func (k DispatchPriorityKey) before(o DispatchPriorityKey) bool {
	if k.Class != o.Class {
		return k.Class < o.Class
	}
	if k.Priority != o.Priority {
		return k.Priority < o.Priority
	}
	if !k.Since.Equal(o.Since) {
		return k.Since.Before(o.Since)
	}
	return k.RunID < o.RunID
}

// agentPriorityKey builds the run's queue key from its CR: class from the
// work-item source label, priority 0 until C2 carries the board priority
// onto the Run, and the restart-stable wait start — the
// WaitingForAgentSlot condition's LastTransitionTime once the run has queued
// (stamped durably on first refusal), else the Run creationTimestamp (the
// ISI-7501 pattern: both facts outlive an operator restart, so a re-driving
// waiter re-registers with the queue position it held before the roll).
func agentPriorityKey(run *api.Run) DispatchPriorityKey {
	k := DispatchPriorityKey{
		Class: dispatchClassForRun(run),
		Since: run.CreationTimestamp.Time,
		RunID: string(run.UID),
	}
	if c := meta.FindStatusCondition(run.Status.Conditions, ConditionAgentSlotWait); c != nil && c.Status == metav1.ConditionTrue {
		k.Since = c.LastTransitionTime.Time
	}
	return k
}

// agentKey namespaces an agent to its team: agent names are team-scoped, and
// the same display name in two squads is two different workers.
func agentKey(team, agent string) string {
	return team + "/" + agent
}

// AgentGate serializes dispatch per (team, agent): at most one in-flight run
// per agent, everyone else queued by DispatchPriorityKey. The zero-value
// maps are unusable — construct with NewAgentGate.
type AgentGate struct {
	mu sync.Mutex
	// inflight maps agentKey(team, agent) to the run uid currently holding
	// the agent's single dispatch slot.
	inflight map[string]string
	// waiters maps agentKey to the runs queued behind the in-flight holder,
	// each with its priority key and last-seen poll time.
	waiters map[string]map[string]*agentWaiter
	// now is the injectable clock (time.Now in production; overridden in
	// tests to exercise TTL eviction and ordering without real sleeps).
	now func() time.Time
	// waiterTTL is agentWaiterTTL, overridable in tests.
	waiterTTL time.Duration
}

// agentWaiter is one run's place in an agent's admission queue: the priority
// key it registered with plus its last-seen poll refresh.
type agentWaiter struct {
	key      DispatchPriorityKey
	lastSeen time.Time
}

// NewAgentGate returns a ready gate.
func NewAgentGate() *AgentGate {
	return &AgentGate{
		inflight:  map[string]string{},
		waiters:   map[string]map[string]*agentWaiter{},
		now:       time.Now,
		waiterTTL: agentWaiterTTL,
	}
}

// Acquire takes the agent's single dispatch slot for runID, or refuses and
// registers the run as a waiter. Idempotent for the holder: the same run
// re-acquiring its own slot (re-drive / retry lap / follow re-attach) is
// always granted — that idempotent re-grant IS sticky admission (D4): no
// reconcile lap, and no later higher-priority arrival, can demote an
// admitted run. A refused run is told the current holder for its wait
// message. Admission on a free slot goes to the best queued run
// (DispatchPriorityKey order) — the caller is granted only when it IS that
// run, so a freed slot never jumps a better-queued waiter.
func (g *AgentGate) Acquire(team, agent, runID string, key DispatchPriorityKey) (holder string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	ak := agentKey(team, agent)
	if cur := g.inflight[ak]; cur != "" {
		if cur == runID {
			// Already admitted: idempotent re-grant, and never also a waiter.
			g.dropWaiter(ak, runID)
			return runID, true
		}
		// NON-PREEMPTION (D3): the incumbent keeps the slot no matter what
		// the newcomer's class/priority is. Register (or refresh) the
		// newcomer as a waiter and name the holder.
		g.registerWaiter(ak, runID, key)
		return cur, false
	}

	now := g.now()
	g.evictStaleWaiters(ak, now)
	g.registerWaiter(ak, runID, key)

	if g.frontWaiter(ak) == runID {
		g.dropWaiter(ak, runID)
		g.inflight[ak] = runID
		return runID, true
	}
	// Slot free but reserved for a better-queued waiter: name it.
	return g.frontWaiter(ak), false
}

// registerWaiter records runID in the agent's queue with its priority key,
// refreshing lastSeen on every poll. A re-registration may UPGRADE the key
// (C2's priority stamp landing after the first wait) but the restart-stable
// Since is re-derived from the same CR facts each time, so it never thrashes.
func (g *AgentGate) registerWaiter(ak, runID string, key DispatchPriorityKey) {
	ws := g.waiters[ak]
	if ws == nil {
		ws = map[string]*agentWaiter{}
		g.waiters[ak] = ws
	}
	if w, ok := ws[runID]; ok {
		w.lastSeen = g.now()
		w.key = key
		return
	}
	ws[runID] = &agentWaiter{key: key, lastSeen: g.now()}
}

// dropWaiter removes runID from the agent's queue, pruning empty maps.
func (g *AgentGate) dropWaiter(ak, runID string) {
	ws := g.waiters[ak]
	if ws == nil {
		return
	}
	delete(ws, runID)
	if len(ws) == 0 {
		delete(g.waiters, ak)
	}
}

// evictStaleWaiters reclaims the queue from waiters that stopped polling
// past waiterTTL (vanished without a terminal Release).
func (g *AgentGate) evictStaleWaiters(ak string, now time.Time) {
	ws := g.waiters[ak]
	for id, w := range ws {
		if now.Sub(w.lastSeen) > g.waiterTTL {
			delete(ws, id)
		}
	}
	if len(ws) == 0 {
		delete(g.waiters, ak)
	}
}

// frontWaiter returns the best-queued (DispatchPriorityKey-first) run uid for
// the agent, or "" when the queue is empty.
func (g *AgentGate) frontWaiter(ak string) string {
	front := ""
	var best DispatchPriorityKey
	have := false
	for id, w := range g.waiters[ak] {
		if !have || w.key.before(best) {
			front, best, have = id, w.key, true
		}
	}
	return front
}

// Release frees the agent slot runID holds (if any) and drops its waiter
// entries everywhere. Idempotent and safe for a run that holds nothing — the
// terminal paths call it unconditionally, which is also how a queued-then-
// terminal run yields its queue place. The freed slot is NOT handed to a
// waiter here: the queue admits on the waiter's own next poll (the 10s
// requeue every waiter rides), keeping admission single-threaded through
// Acquire.
func (g *AgentGate) Release(runID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for ak, cur := range g.inflight {
		if cur == runID {
			delete(g.inflight, ak)
		}
	}
	for ak := range g.waiters {
		g.dropWaiter(ak, runID)
	}
}

// Holder reports the run uid currently holding the (team, agent) slot, or ""
// when the agent is free (test/observability surface).
func (g *AgentGate) Holder(team, agent string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight[agentKey(team, agent)]
}

// ReclaimInflight evicts every in-flight holder the caller reports as no
// longer live (terminal Run or Run CR gone) and returns the evicted agent
// keys + run uids — the agent-axis twin of the EndpointGate's ReclaimHolders
// (ISI-7483): every release site fires from the driver's terminal paths, so
// a run that terminalizes through any OTHER path would leak its agent slot
// and starve the agent's queue forever. live TRUE = keep; an ERROR also
// counts as live — never reclaim on a maybe.
func (g *AgentGate) ReclaimInflight(live func(runID string) (bool, error)) []EvictedHolder {
	g.mu.Lock()
	defer g.mu.Unlock()
	var evicted []EvictedHolder
	for ak, id := range g.inflight {
		isLive, err := live(id)
		if err != nil || isLive {
			continue // fail-safe: never reclaim on a maybe
		}
		delete(g.inflight, ak)
		evicted = append(evicted, EvictedHolder{Endpoint: ak, RunID: id})
	}
	return evicted
}

// queuedAwaitingDispatch reports whether the Run CR carries any LIVE dispatch
// queue-wait marker — the legible conditions the driver/dispatcher stamp
// while the run is parked in one of the three dispatch queues (BYO endpoint
// slot, per-agent slot, Supervisor mesh saturation). Such a run is WAITING,
// not wedged and not dying.
func queuedAwaitingDispatch(run *api.Run) bool {
	for _, cond := range []string{ConditionEndpointSlotWait, ConditionAgentSlotWait, ConditionQueuedAtSupervisor} {
		if c := meta.FindStatusCondition(run.Status.Conditions, cond); c != nil && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// neverDispatched reports whether the run has no a2a dispatch settlement at
// all — it never reached an agent (the never-dispatched queue-waiter shape
// the C1 evidence terminalized). With no Settle reader wired (unit tests,
// ledger-only lanes) it fails toward true for a queued run, preserving the
// guard for test doubles; production always wires Settle.
func (r *Driver) neverDispatched(ctx context.Context, run *api.Run) (bool, error) {
	if r.Settle == nil {
		return true, nil
	}
	dispatched, _, _, err := r.Settle.RunFollowOutcome(ctx, string(run.UID))
	if err != nil {
		return false, err
	}
	return !dispatched, nil
}
