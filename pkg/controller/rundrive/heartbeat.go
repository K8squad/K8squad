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

// heartbeat.go — the §6.2/§6.3 claim hygiene sweep (ISI-4183, M1.3). With the
// drive loop now claiming work items, two follow-on duties keep the checkout
// world honest — both level-triggered sweeps in the CancelSweeper shape, so a
// missed tick costs delay, never correctness:
//
//   - RENEW: a held, in-flight claim's lease is short (30s, DefaultProdConfig)
//     so a dead holder cannot wedge an item for long. Real agent runs outlive
//     it by minutes — without renewal, the 3.2 death detector would re-enter
//     every healthy long run as a "dead holder" retry lap. The sweep renews
//     every live in-flight lease each tick.
//   - RELEASE: a Run that reached a terminal step still holds its checkout
//     until someone clears it (the machine commits the step; custody release
//     is bookkeeping after the fact). The sweep releases held terminal rows —
//     custody only: the LANE stays in_progress on success (M1.5's reporting
//     owns lane moves on completion) and returns to todo on failure/cancel
//     (the item is reclaimable — the same lane-return the re-enter paths
//     co-commit).
package rundrive

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/K8squad/K8squad/pkg/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runExistenceChecker answers "does the backing Run CR still exist?" (ISI-5184).
// Blind renewal kept zombie claims alive for Run CRs deleted out from under the
// sweep — the verifier refuses to renew custody whose provenance is gone and
// hands the row to the orphan-release path instead.
type runExistenceChecker interface {
	RunExists(ctx context.Context, runID string) (bool, error)
}

// LiveRuns is the production runExistenceChecker: an uncached API-reader Get
// keyed on the Run's UID. Delete-then-recreate is indistinguishable from
// existence by name, so the check is UID-exact — a recreated Run with the same
// name is a DIFFERENT run and must not legitimize the old claim.
type LiveRuns struct {
	Reader client.Reader
}

// RunExists reports whether a Run CR with the given UID exists in any watched
// namespace. A malformed runID is treated as non-existent (a claim with broken
// provenance is an orphan by definition).
func (l *LiveRuns) RunExists(ctx context.Context, runID string) (bool, error) {
	if l == nil || l.Reader == nil || runID == "" {
		return false, nil
	}
	runs := &ksquadv1alpha1.RunList{}
	if err := l.Reader.List(ctx, runs); err != nil {
		return false, fmt.Errorf("rundrive.heartbeat: list runs: %w", err)
	}
	for i := range runs.Items {
		if string(runs.Items[i].UID) == runID {
			return true, nil
		}
	}
	return false, nil
}

// runLivenessChecker answers "does this Run still have a live sandbox pod?" for
// the zombie-run gate (ISI-5438). A Run CR can outlive its sandbox — the pod is
// evicted, the node is lost, or a bind never completed — yet the Run stays in a
// non-terminal step with its coord.claim lease renewed. The ISI-5184 RunExists
// gate only catches a DELETED Run CR; a zombie whose CR still exists but whose
// pod is gone was renewed forever, so the 3.2 death detector never fired and the
// workspace stayed wedged (the 34 live Claiming/Running zombies on bmad-demo-project
// with no backing pod). Nil keeps the pre-ISI-5438 behavior (renew blindly).
type runLivenessChecker interface {
	HasLiveSandbox(ctx context.Context, runID string) (bool, error)
}

// runRunningChecker is the STRICTER holder-side pod bar (ISI-7788). Where
// runLivenessChecker (the claim-side zombie gate) counts any existing pod
// as live so a booting claim is never reaped, the holder-side reclaim must
// not accept a pod that has existed without ever going Running: past the
// bring-up grace that is a stuck sandbox bring-up holding a BYO endpoint
// permit without dispatching (the ISI-7788 wedge — a Claiming holder with a
// never-Ready pod parked a 1-slot endpoint while every waiter starved).
type runRunningChecker interface {
	HasRunningSandbox(ctx context.Context, runID string) (bool, error)
}

// HasLiveSandbox reports whether any non-terminating, non-terminal sandbox pod
// carries the ksquad.io/run label for the given Run UID. Zero matching live pods
// ⇒ the Run's sandbox is gone. A list error is returned to the caller, which
// fails SAFE by keeping the claim (ISI-5438: never release a zombie on a maybe —
// the warm-pool reaper's "never reap on a maybe" discipline applied to custody).
func (l *LiveRuns) HasLiveSandbox(ctx context.Context, runID string) (bool, error) {
	if l == nil || l.Reader == nil || runID == "" {
		return false, nil
	}
	pods := &corev1.PodList{}
	if err := l.Reader.List(ctx, pods, client.MatchingLabels{sandbox.LabelRun: runID}); err != nil {
		return false, fmt.Errorf("rundrive.heartbeat: list sandbox pods for run %s: %w", runID, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue // terminating — not live
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue // terminated — not live
		}
		// Pending (scheduling/pulling) and Running both count as live: a pod that
		// physically exists is NOT a zombie, so a capacity-starved Claiming run is
		// never wrongly reaped (only a Run with NO pod at all is a zombie).
		return true, nil
	}
	return false, nil
}

// HasRunningSandbox is the STRICTER bar the holder-side reclaim applies
// (ISI-7788): only a pod whose phase is Running counts. HasLiveSandbox
// deliberately counts a Pending (scheduling/pulling) pod as live — the
// claim-side zombie gate must not reap a capacity-starved boot — but that
// leniency let a wedged holder park a single-slot BYO endpoint behind a pod
// that existed yet never became Ready: terminal-or-gone never fired (the
// Run CR is non-terminal), the pod-liveness half never fired (a pod exists),
// and every waiter starved behind a non-dispatching holder. Past the
// bring-up grace a holder must prove a RUNNING pod. Fails SAFE on a list
// error (keep — never reclaim on a maybe); fails CLOSED (false, nil) on an
// empty runID.
func (l *LiveRuns) HasRunningSandbox(ctx context.Context, runID string) (bool, error) {
	if l == nil || l.Reader == nil || runID == "" {
		return false, nil
	}
	pods := &corev1.PodList{}
	if err := l.Reader.List(ctx, pods, client.MatchingLabels{sandbox.LabelRun: runID}); err != nil {
		return false, fmt.Errorf("rundrive.heartbeat: list sandbox pods for run %s: %w", runID, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue // terminating — not running
		}
		if p.Status.Phase == corev1.PodRunning {
			return true, nil
		}
	}
	return false, nil
}

// endpointWaitChecker answers "is this Run currently queued behind a busy BYO
// model endpoint slot?" for the ISI-5591 reaper exemption. A run waiting on a
// single-slot BYO endpoint (the discussion/party-mode case: N agents serialized
// through one deepseek-endpoint slot) intentionally has NO sandbox pod — the
// driver queues it OUTSIDE the endpoint via waitEndpointSlot and dispatches
// nothing until the slot frees, because booting before it holds a slot would
// kill it on the shim's first-token watchdog. So "pod-less past the stall grace"
// is the EXPECTED steady state of a legitimate endpoint-waiter, and the ISI-5438
// pod-liveness zombie gate would otherwise reap it the moment it waits >grace.
// Nil disables the exemption (pre-ISI-5591 behavior).
type endpointWaitChecker interface {
	IsWaitingForEndpointSlot(ctx context.Context, runID string) (bool, error)
}

// IsWaitingForEndpointSlot reports whether the Run with the given UID currently
// carries a True WaitingForEndpointSlot condition (ISI-5591) — the legible-wait
// marker the driver stamps (ConditionEndpointSlotWait / Reason=EndpointBusy)
// while the run is queued on a full BYO endpoint. Such a run is making forward
// progress by holding its FIFO position, not wedged: it must NOT be reaped as a
// pod-less zombie, force-failed, and re-minted (r→r+1) every ~10 min. A list
// error is returned so the reaper fails SAFE toward the waiter (never reap on a
// maybe). Fails CLOSED (false,nil) on an empty runID or a Run whose UID is gone.
func (l *LiveRuns) IsWaitingForEndpointSlot(ctx context.Context, runID string) (bool, error) {
	if l == nil || l.Reader == nil || runID == "" {
		return false, nil
	}
	runs := &ksquadv1alpha1.RunList{}
	if err := l.Reader.List(ctx, runs); err != nil {
		return false, fmt.Errorf("rundrive.heartbeat: list runs: %w", err)
	}
	for i := range runs.Items {
		if string(runs.Items[i].UID) != runID {
			continue
		}
		c := meta.FindStatusCondition(runs.Items[i].Status.Conditions, ConditionEndpointSlotWait)
		return c != nil && c.Status == metav1.ConditionTrue, nil
	}
	return false, nil
}

// endpointSlotGate is the sweep-facing subset of *EndpointGate (ISI-7483):
// reclaim the permits of dead holders and release a run's slots outright.
type endpointSlotGate interface {
	ReclaimHoldersSince(live func(runID string, heldSince time.Time) (bool, error)) []EvictedHolder
	ReleaseByRun(runID string)
}

// agentReclaimGate is the sweep-facing subset of *AgentGate (ISI-7499 C4):
// reclaim the dispatch slot of a dead in-flight holder.
type agentReclaimGate interface {
	ReclaimInflight(live func(runID string) (bool, error)) []EvictedHolder
	Release(runID string)
}

// runTerminalChecker answers "is this run uid terminal or gone?" for the
// ISI-7483 holder reclaim (satisfied by *LiveRuns.TerminalOrGone).
type runTerminalChecker interface {
	TerminalOrGone(ctx context.Context, runID string) (bool, error)
}

// runTerminalBatchChecker is the batched form of runTerminalChecker
// (*LiveRuns.TerminalOrGoneAll): one unselected RunList per sweep pass,
// verdicts indexed by UID, instead of one full list PER HOLDER — the sweep
// checks many holders every 10s through the uncached API reader.
type runTerminalBatchChecker interface {
	TerminalOrGoneAll(ctx context.Context) (map[string]bool, error)
}

// agentSlotWaitChecker answers "is this Run queued behind its busy agent?"
// for the zombie-reap exemption (ISI-7499 C4). A run parked on the
// per-agent non-preemptive dispatch queue intentionally has NO sandbox pod —
// the driver parks it pod-less at the wait cadence with the
// WaitingForAgentSlot condition until the agent's in-flight run goes
// terminal — the exact pod-less predicate the ISI-5438 zombie gate reaps.
// Nil disables the exemption.
type agentSlotWaitChecker interface {
	IsWaitingForAgentSlot(ctx context.Context, runID string) (bool, error)
}

// IsWaitingForAgentSlot reports whether the Run with the given UID currently
// carries a True WaitingForAgentSlot condition (ISI-7499 C4) — the
// legible-wait marker the driver stamps (ConditionAgentSlotWait /
// Reason=AgentBusy) while the run is queued on its agent's single dispatch
// slot. Such a run holds a real queue position, it is not wedged: it must
// NOT be reaped as a pod-less zombie, force-failed, and re-minted. A list
// error is returned so the reaper fails SAFE toward the waiter (never reap
// on a maybe). Fails CLOSED (false,nil) on an empty runID or a Run whose UID
// is gone.
func (l *LiveRuns) IsWaitingForAgentSlot(ctx context.Context, runID string) (bool, error) {
	if l == nil || l.Reader == nil || runID == "" {
		return false, nil
	}
	runs := &ksquadv1alpha1.RunList{}
	if err := l.Reader.List(ctx, runs); err != nil {
		return false, fmt.Errorf("rundrive.heartbeat: list runs: %w", err)
	}
	for i := range runs.Items {
		if string(runs.Items[i].UID) != runID {
			continue
		}
		c := meta.FindStatusCondition(runs.Items[i].Status.Conditions, ConditionAgentSlotWait)
		return c != nil && c.Status == metav1.ConditionTrue, nil
	}
	return false, nil
}

// meshQueueWaitChecker answers "is this Run queued at the Supervisor on a
// saturated mesh pool?" for the S4 zombie-reap exemption (ISI-5831, chapter
// §5.4 D9.1). A mesh-routed Run whose dispatch was refused with 409 (every
// standing process busy) sits in the Supervisor's re-drive queue with NO
// sandbox pod — mesh runs never boot one — which is exactly the pod-less
// predicate the ISI-5438 zombie gate reaps. The QueuedAtSupervisor condition
// distinguishes "dispatched but waiting on Supervisor saturation" from
// "executing" (and from "wedged"), so the zombie-Claiming reconciler
// (ISI-5541/5542 class) does not mis-fire on it. Nil disables the exemption.
type meshQueueWaitChecker interface {
	IsQueuedAtSupervisor(ctx context.Context, runID string) (bool, error)
}

// IsQueuedAtSupervisor reports whether the Run with the given UID currently
// carries a True QueuedAtSupervisor condition (ISI-5831) — the legible-wait
// marker the dispatcher stamps while the run holds its re-drive queue
// position behind a saturated mesh pool. Such a run is making forward
// progress (the pool's processes are draining), not wedged: it must NOT be
// reaped as a pod-less zombie, force-failed, and re-minted every ~10 min. A
// list error is returned so the reaper fails SAFE toward the waiter (never
// reap on a maybe). Fails CLOSED (false,nil) on an empty runID or a Run
// whose UID is gone.
func (l *LiveRuns) IsQueuedAtSupervisor(ctx context.Context, runID string) (bool, error) {
	if l == nil || l.Reader == nil || runID == "" {
		return false, nil
	}
	runs := &ksquadv1alpha1.RunList{}
	if err := l.Reader.List(ctx, runs); err != nil {
		return false, fmt.Errorf("rundrive.heartbeat: list runs: %w", err)
	}
	for i := range runs.Items {
		if string(runs.Items[i].UID) != runID {
			continue
		}
		c := meta.FindStatusCondition(runs.Items[i].Status.Conditions, ConditionQueuedAtSupervisor)
		return c != nil && c.Status == metav1.ConditionTrue, nil
	}
	return false, nil
}

// terminalOrGoneVerdict is the per-Run verdict behind TerminalOrGone and
// TerminalOrGoneAll. ISI-7687: a Terminating CR (DeletionTimestamp set) counts
// as gone — a deleting Run can hold nothing — instead of waiting for it to
// vanish from the list (a deletion held up past the projector's status-patch
// error kept the holder "live" otherwise). NOTE: this predicate is SHARED —
// cmd/operator/main.go also wires it as the warm-pool terminal reaper's
// oracle (warmpool.SweepTerminalRunOwned DELETES the pod on a true verdict),
// so the Terminating case additionally tears down a deleting Run's sandbox;
// and between the verdict and the pod's actual termination the gate may admit
// a second run onto a 1-slot endpoint while the first is still streaming.
func terminalOrGoneVerdict(run *ksquadv1alpha1.Run) bool {
	if run.DeletionTimestamp != nil {
		return true
	}
	switch run.Status.Phase {
	case ksquadv1alpha1.RunPhaseSucceeded,
		ksquadv1alpha1.RunPhaseFailed,
		ksquadv1alpha1.RunPhaseCancelled:
		return true // terminal: its sandbox is leaked
	default:
		return false // Pending/Claiming/Running/Paused/Canceling: keep
	}
}

// TerminalOrGone reports whether the Run owning a sandbox pod has reached a
// terminal phase (Succeeded/Failed/Cancelled) or no longer exists (ISI-5438).
// A sandbox whose Run is terminal is LEAKED — the a2a-settlement reaper
// (warmpool.reapIfSettled) misses it because a Cancelled run never writes an
// a2a settled_at, so the in-memory Pool.Release is the only teardown and it
// no-ops after an operator restart (the live 2d5h leaked `sandbox-a16f11a0…`
// held the project RWO PVC hostage). The warm-pool terminal reaper uses this to
// garbage-collect such pods. The UID is read from the pod's AnnRunID stamp, so
// a delete-then-recreate with the same name is a DIFFERENT run and the stale
// pod is correctly judged gone. Fails CLOSED (false,nil) on an empty runID.
func (l *LiveRuns) TerminalOrGone(ctx context.Context, runID string) (bool, error) {
	if l == nil || l.Reader == nil || runID == "" {
		return false, nil // no provable run id ⇒ cannot decide ⇒ keep
	}
	runs := &ksquadv1alpha1.RunList{}
	if err := l.Reader.List(ctx, runs); err != nil {
		return false, fmt.Errorf("rundrive.heartbeat: list runs: %w", err)
	}
	for i := range runs.Items {
		if string(runs.Items[i].UID) != runID {
			continue
		}
		return terminalOrGoneVerdict(&runs.Items[i]), nil
	}
	return true, nil // Run CR with this UID is gone: the sandbox is orphaned
}

// TerminalOrGoneAll is the batched form of TerminalOrGone (ISI-7687): ONE
// unselected RunList per call, returning the terminal-or-gone verdict per Run
// UID. The holder-reclaim sweep checks many holders per 10s tick through the
// uncached API reader, so per-holder lists made the sweep O(holders x all
// Runs) in API round trips; a UID missing from the map is gone (same verdict
// TerminalOrGone returns for an absent CR).
func (l *LiveRuns) TerminalOrGoneAll(ctx context.Context) (map[string]bool, error) {
	if l == nil || l.Reader == nil {
		return nil, nil
	}
	runs := &ksquadv1alpha1.RunList{}
	if err := l.Reader.List(ctx, runs); err != nil {
		return nil, fmt.Errorf("rundrive.heartbeat: list runs: %w", err)
	}
	out := make(map[string]bool, len(runs.Items))
	for i := range runs.Items {
		out[string(runs.Items[i].UID)] = terminalOrGoneVerdict(&runs.Items[i])
	}
	return out, nil
}

// HeartbeatInterval is the keepalive tick. It is comfortably inside the 30s
// claim lease (a single missed tick must not lapse a healthy run's lease),
// while the sweep itself is one cheap indexed query over the small claim
// table.
const HeartbeatInterval = 10 * time.Second

// DefaultZombieStallGrace is how long a non-terminal held claim may go WITHOUT a
// live sandbox pod before the sweep treats it as a zombie and stops renewing
// (ISI-5438). It must comfortably exceed a cold sandbox boot (image pull +
// schedule + Ready) so a legitimately-Claiming Run is never reaped mid-boot; the
// zombies this targets have been pod-less for days, so a generous grace costs
// nothing. Measured from coord.claim.acquired_at.
const DefaultZombieStallGrace = 10 * time.Minute

// renewer is the §6.2 lease-renewal seam the sweep depends on (satisfied by
// *coord.ProdClaimer; faked in tests).
type renewer interface {
	Renew(ctx context.Context, itemID, principal, runID string, fence int64) bool
}

// HeartbeatSweeper is a manager Runnable: every tick, renew the leases of
// in-flight held claims and release the checkouts of terminal held claims.
type HeartbeatSweeper struct {
	// DB is the coordination Postgres (required).
	DB *sql.DB
	// Claimer executes the §6.2 renew (required).
	Claimer renewer
	// Runs verifies the backing Run CR still exists before renewal (ISI-5184).
	// Nil keeps the pre-ISI-5184 blind-renew behavior (tests, legacy wiring).
	Runs runExistenceChecker
	// Live verifies the backing Run still has a live sandbox pod before renewal
	// (ISI-5438). Nil keeps the pre-ISI-5438 behavior (renew any live Run CR).
	Live runLivenessChecker
	// EndpointWait exempts a pod-less in-flight claim from the ISI-5438 zombie
	// reap when its Run is legitimately queued behind a busy single-slot BYO
	// endpoint (ISI-5591 / WaitingForEndpointSlot). Nil disables the exemption
	// (pre-ISI-5591 behavior; tests opt in explicitly).
	EndpointWait endpointWaitChecker
	// MeshQueueWait exempts a pod-less in-flight claim from the ISI-5438
	// zombie reap when its Run is legitimately queued at the Supervisor
	// behind a saturated mesh pool (ISI-5831 / QueuedAtSupervisor — S4,
	// D9.1). Nil disables the exemption (pre-S4 behavior; tests opt in).
	MeshQueueWait meshQueueWaitChecker
	// AgentWait exempts a pod-less in-flight claim from the ISI-5438 zombie
	// reap when its Run is legitimately queued behind its busy agent on the
	// per-agent non-preemptive dispatch queue (ISI-7499 C4 /
	// WaitingForAgentSlot). Nil disables the exemption.
	AgentWait agentSlotWaitChecker
	// EndpointGate is the per-BYO-endpoint slot gate (ISI-5037). When set
	// (with HolderTerminal) each sweep pass reclaims slots whose holder run
	// is terminal or gone — the ISI-7483 backstop for permits leaked by
	// failure paths that never reach the gate's release sites (a Failed run
	// held a single-slot endpoint for 47+ minutes while every queued seat
	// sat "exempt from zombie reap"). Nil disables the reclaim
	// (pre-ISI-7483 behavior; tests opt in explicitly).
	EndpointGate endpointSlotGate
	// AgentGate is the per-agent non-preemptive dispatch gate (ISI-7499
	// C4). When set (with HolderTerminal) each sweep pass reclaims the
	// agent slot of a holder run that terminalized through a path the
	// driver's release sites missed — the agent-axis twin of the ISI-7483
	// endpoint backstop. Nil disables the reclaim.
	AgentGate agentReclaimGate
	// HolderTerminal reports whether a run uid is terminal or its Run CR is
	// gone (*LiveRuns.TerminalOrGone). Nil disables the reclaim.
	HolderTerminal runTerminalChecker
	// HolderSandbox answers whether a holder run has a RUNNING sandbox pod
	// (ISI-7687/ISI-7788 — *LiveRuns.HasRunningSandbox). A holder whose Run
	// CR is non-terminal but whose sandbox is absent — or stuck in a
	// never-Ready Pending state — past the stall grace (anchored on the
	// gate's frozen acquisition time) is a zombie holder: its slot is
	// reclaimed so queued waiters proceed. ISI-7788 tightened the bar from
	// "a pod exists" to "a pod is Running": a pod that exists but never
	// boots is a stuck bring-up, not proof of endpoint use (a Claiming
	// holder wedged a 1-slot BYO endpoint for 14+ min while 12 waiters
	// starved with zero endpoint throughput). Nil disables the pod-liveness
	// half (terminal-or-gone reclaim only — pre-ISI-7687 behavior).
	HolderSandbox runRunningChecker
	// ZombieStallGrace overrides DefaultZombieStallGrace when > 0 (tests shrink
	// it). A pod-less in-flight claim older than this is released as a zombie.
	ZombieStallGrace time.Duration
	// Now overrides the clock for the stall measurement (nil ⇒ time.Now).
	Now func() time.Time
	// Tick overrides HeartbeatInterval when > 0 (tests shrink it).
	Tick time.Duration
	// Log receives diagnostics (nil discards).
	Log func(format string, args ...any)
}

// heldClaim is one row of the sweep's backlog.
type heldClaim struct {
	workItemID string
	holderRun  string
	principal  string
	fence      int64
	step       reconcile.Step
	// acquiredAt is when custody was taken; the zombie stall grace (ISI-5438) is
	// measured from it. Zero when NULL (legacy rows) — such a row is never
	// flagged as a zombie (the stall check fails closed without an anchor).
	acquiredAt time.Time
}

// Start runs the sweep until ctx is done. Errors are logged and retried next
// tick — a transient DB stall must never kill the sweeper (nor the operator:
// the §6.2 Renew contract panics on infrastructure failure, so each call is
// contained and reported as a log line instead).
func (s *HeartbeatSweeper) Start(ctx context.Context) error {
	tick := s.Tick
	if tick <= 0 {
		tick = HeartbeatInterval
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

// sweep is one level-triggered pass over every held claim row.
func (s *HeartbeatSweeper) sweep(ctx context.Context) {
	// ISI-7483: free BYO endpoint slots held by dead runs FIRST — the
	// reclaim must not wait on the claim backlog (nor on a DB stall).
	s.reclaimDeadHolders(ctx)

	held, err := s.due(ctx)
	if err != nil {
		s.logf("rundrive.heartbeat: %v", err)
		return
	}
	for _, hc := range held {
		switch {
		case reconcile.IsTerminal(hc.step):
			// Terminal Run still holding its checkout: release custody. The
			// guard is the holder run id + terminal step, so the release is
			// idempotent and crash-safe — a re-sweep of an already-released
			// row simply matches nothing.
			if err := s.releaseTerminal(ctx, hc); err != nil {
				s.logf("rundrive.heartbeat: release %s: %v", hc.workItemID, err)
			}
		default:
			// In flight: renew the lease — but only if the backing Run CR
			// still exists (ISI-5184). Blind renewal wedged workspaces behind
			// zombie claims whose Run had been deleted with no terminal step
			// ever committed: the sweep renewed forever, the lane stayed
			// in_progress, and the busy fallback served an empty listing.
			if s.Runs != nil && hc.holderRun != "" {
				live, err := s.Runs.RunExists(ctx, hc.holderRun)
				if err != nil {
					// Verification unavailable: fail safe by NOT renewing —
					// a skipped tick costs delay (the 3.2 death detector can
					// still reclaim a lapsed lease), renewing a zombie does
					// not. Retry is the next level-triggered tick.
					s.logf("rundrive.heartbeat: verify %s run %s: %v — renewal skipped this tick",
						hc.workItemID, hc.holderRun, err)
					continue
				}
				if !live {
					if err := s.releaseOrphan(ctx, hc); err != nil {
						s.logf("rundrive.heartbeat: orphan-release %s: %v", hc.workItemID, err)
					} else {
						s.logf("rundrive.heartbeat: released orphan claim %s (run %s no longer exists, step %s)",
							hc.workItemID, hc.holderRun, hc.step)
					}
					continue
				}
			}
			// ISI-5438 zombie gate: the Run CR exists (or verification is
			// off) — but does it still have a live sandbox pod? A Run wedged in a
			// non-terminal step with NO pod past the stall grace is a zombie
			// whose lease blind renewal keeps fresh forever — the 3.2 death
			// detector never fires because the lease never lapses. Stop
			// renewing and release it as failed (lane → todo, reclaimable).
			// Paused runs are skipped: park() keeps their pod and a long
			// credential/rate-limit wait is intentional, not a zombie.
			if s.Live != nil && hc.holderRun != "" && !isPausedStep(hc.step) && s.stalled(hc) {
				live, err := s.Live.HasLiveSandbox(ctx, hc.holderRun)
				if err != nil {
					// Pod liveness unknown: never release on a maybe — fall
					// through to renew and retry next level-triggered tick.
					s.logf("rundrive.heartbeat: sandbox liveness for %s run %s: %v — renewing, zombie check deferred",
						hc.workItemID, hc.holderRun, err)
				} else if !live && !s.endpointSlotWaiter(ctx, hc) && !s.meshQueueWaiter(ctx, hc) && !s.agentSlotWaiter(ctx, hc) {
					// Pod-less past the grace AND not a legitimate endpoint-slot
					// waiter (ISI-5591) AND not a Supervisor-queued mesh run
					// (ISI-5831 S4) AND not an agent-queue waiter (ISI-7499
					// C4): a genuine zombie. Release it as failed.
					if err := s.releaseZombie(ctx, hc); err != nil {
						s.logf("rundrive.heartbeat: zombie-release %s: %v", hc.workItemID, err)
					} else {
						s.logf("rundrive.heartbeat: released zombie claim %s (run %s has no live sandbox pod, step %s, held %s)",
							hc.workItemID, hc.holderRun, hc.step, s.clock().Sub(hc.acquiredAt).Round(time.Second))
					}
					continue
				}
			}
			// Renew's own SQL guard (lease_expires_at > clock_timestamp())
			// refuses an already-lapsed lease — the authoritative liveness
			// check — and a lapsed one is the 3.2 death detector's business
			// on the Run's next pass.
			s.renew(ctx, hc)
		}
	}
}

// reclaimDeadHolders frees BYO endpoint slots held by runs that are terminal
// or whose Run CR is gone (ISI-7483). This is the holder-side backstop of the
// ISI-5037 gate: the release sites all fire from the dispatch/driver paths, so
// a run terminalized by the projector, a reaper, or an errored-follow +
// budget-exhaustion sequence leaked its in-process permit forever — and one
// dead holder starved every waiter on a single-slot endpoint while the
// ISI-5591 waiter exemption kept their claims alive. Runs BEFORE the claim
// backlog walk (a DB stall must not delay the reclaim) and fails safe: a
// liveness error counts as live (never reclaim on a maybe).
func (s *HeartbeatSweeper) reclaimDeadHolders(ctx context.Context) {
	if s.HolderTerminal == nil || (s.EndpointGate == nil && s.AgentGate == nil) {
		return
	}
	// ISI-7687 review: hoist the RunList to ONCE per sweep pass (indexed by
	// UID) instead of one unselected list per holder — the gate now calls
	// this predicate outside its mutex, but the per-holder list was still
	// O(holders x all Runs) in API round trips every 10s.
	verdicts, verdictErr := s.terminalVerdicts(ctx)
	live := func(runID string, heldSince time.Time) (bool, error) {
		var dead bool
		if verdicts != nil {
			if verdictErr != nil {
				return true, verdictErr // fail-safe: never reclaim on a maybe
			}
			var ok bool
			dead, ok = verdicts[runID]
			if !ok {
				// ISI-7770: absent from the batch snapshot is NOT proof the Run CR is gone — the holder may
				// have acquired its slot AFTER the sweep's RunList was taken (the 20:42:35Z four-run
				// false-positive storm reclaimed brand-new holders 3s after creation, and the evicted runs
				// kept executing). Re-check this one run before reclaiming: a fresh TerminalOrGone verdict
				// is authoritative, the batch snapshot stays an optimization. A run that is genuinely
				// absent there is gone; a fresh non-terminal CR keeps its slot.
				d, err := s.HolderTerminal.TerminalOrGone(ctx, runID)
				if err != nil {
					return true, err // fail-safe: never reclaim on a maybe
				}
				dead = d
			}
		} else {
			d, err := s.HolderTerminal.TerminalOrGone(ctx, runID)
			if err != nil {
				return true, err // fail-safe: never reclaim on a maybe
			}
			dead = d
		}
		if dead {
			return false, nil
		}
		// ISI-7687 liveness gap: terminal-or-gone alone reports a wedged
		// non-terminal holder (status.sandbox=null, never terminalized) as
		// live forever — the 116h single-slot BYO hold. Past the bring-up
		// grace, also require a live sandbox pod.
		return s.holderSandboxLive(ctx, runID, heldSince)
	}
	if s.EndpointGate != nil {
		evicted := s.EndpointGate.ReclaimHoldersSince(live)
		for _, e := range evicted {
			s.logf("rundrive.heartbeat: reclaimed BYO endpoint slot on %s from dead holder run %s — queued waiters proceed (ISI-7483)",
				e.Endpoint, e.RunID)
		}
	}
	if s.AgentGate != nil {
		// Age-blind for the agent axis (ISI-7499 C4 shape): the zero
		// heldSince disables the ISI-7687 sandbox half — the agent slot
		// reclaims on terminal-or-gone only.
		agentEvicted := s.AgentGate.ReclaimInflight(func(id string) (bool, error) {
			return live(id, time.Time{})
		})
		for _, e := range agentEvicted {
			s.logf("rundrive.heartbeat: reclaimed agent dispatch slot %s from dead holder run %s — queued runs proceed (ISI-7499 C4)",
				e.Endpoint, e.RunID)
		}
	}
}

// terminalVerdicts returns the terminal-or-gone verdict for every Run UID in
// ONE RunList when the checker supports the batch form, else falls back to
// per-run TerminalOrGone calls (verdicts stays nil and verdictErr nil, and
// the caller re-checks per run). A list error is returned so the reclaim
// fails SAFE (never reclaim on a maybe).
func (s *HeartbeatSweeper) terminalVerdicts(ctx context.Context) (map[string]bool, error) {
	if b, ok := s.HolderTerminal.(runTerminalBatchChecker); ok {
		return b.TerminalOrGoneAll(ctx)
	}
	return nil, nil // no batch form: per-run fallback in the live closure
}

// holderSandboxLive is the ISI-7687 holder half of the reclaim predicate:
// a holder that has held its slot longer than the stall grace (the same
// DefaultZombieStallGrace the claim-side zombie gate uses, measured from
// the gate's frozen acquisition time) must prove a RUNNING sandbox pod
// (ISI-7788: an existing-but-never-Ready pod is a stuck bring-up, not
// proof of endpoint use) or yield the slot — a sandbox that never became
// Ready must not hold a single-slot BYO endpoint unbounded. Within the grace the holder is kept
// without the pod check (cold-boot allowance). A checker error fails SAFE
// (keep — never reclaim on a maybe); a nil checker or a zero heldSince
// (pre-ISI-7687 gate state) also keeps. NOTE the deliberate asymmetry with
// the claim-side zombie gate above: that gate skips paused steps
// (isPausedStep) because park() keeps the pod during an intentional
// credential/rate-limit wait, while THIS holder-side predicate has no paused
// exemption — park() neither releases the endpoint permit nor touches the
// pod, so a paused holder whose pod vanished is evicted here while the
// claim-side gate keeps the claim. Today a parked run's pod normally survives
// the pause, so the two gates agree in practice; the asymmetry is documented
// here so a later refactor does not "fix" it by accident.
func (s *HeartbeatSweeper) holderSandboxLive(ctx context.Context, runID string, heldSince time.Time) (bool, error) {
	if s.HolderSandbox == nil || heldSince.IsZero() {
		return true, nil
	}
	grace := s.ZombieStallGrace
	if grace <= 0 {
		grace = DefaultZombieStallGrace
	}
	if s.clock().Sub(heldSince) <= grace {
		return true, nil // bring-up window: the sandbox may not exist yet
	}
	// ISI-7788: the holder bar is a RUNNING pod, not mere existence — a
	// pod stuck Pending since acquisition is a wedged bring-up. The
	// claim-side zombie gate above stays lenient (it must not reap a
	// booting claim); the asymmetry is deliberate and documented in
	// LiveRuns.HasRunningSandbox.
	return s.HolderSandbox.HasRunningSandbox(ctx, runID)
}

// releaseGateSlot drops a dead/terminal run's endpoint-gate presence (slots
// AND FIFO waiter entries) as the claim-release paths' belt-and-braces half of
// the ISI-7483 holder backstop. Idempotent; nil-safe.
func (s *HeartbeatSweeper) releaseGateSlot(hc heldClaim) {
	if s.EndpointGate == nil || hc.holderRun == "" {
		return
	}
	s.EndpointGate.ReleaseByRun(hc.holderRun)
}

// due lists every held claim row with its machine step.
func (s *HeartbeatSweeper) due(ctx context.Context) ([]heldClaim, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT work_item_id::text, NULLIF(run_id::text,''), holder_principal, fence_token, reconcile_step, acquired_at
		  FROM coord.claim
		 WHERE holder_principal IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("rundrive.heartbeat: list held claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []heldClaim
	for rows.Next() {
		var hc heldClaim
		var acquired sql.NullTime
		if err := rows.Scan(&hc.workItemID, &hc.holderRun, &hc.principal, &hc.fence, &hc.step, &acquired); err != nil {
			return nil, fmt.Errorf("rundrive.heartbeat: scan held claim: %w", err)
		}
		if acquired.Valid {
			hc.acquiredAt = acquired.Time
		}
		out = append(out, hc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rundrive.heartbeat: held claims cursor: %w", err)
	}
	return out, nil
}

// renew extends one in-flight lease. The §6.2 Renew panics on infrastructure
// failure (its run-loop contract); the sweep contains that panic — a sweeper
// goroutine dying would take the whole operator down over a transient DB
// stall, and the level-triggered next tick is the honest retry.
func (s *HeartbeatSweeper) renew(ctx context.Context, hc heldClaim) {
	defer func() {
		if p := recover(); p != nil {
			s.logf("rundrive.heartbeat: renew %s panicked: %v", hc.workItemID, p)
		}
	}()
	if hc.holderRun == "" {
		return // provenance gap: nothing Renew's (run_id, fence) guard accepts
	}
	if !s.Claimer.Renew(ctx, hc.workItemID, hc.principal, hc.holderRun, hc.fence) {
		// Lost the lease (fence moved / foreign holder / lapsed): the 3.2
		// death detector owns the follow-up on the Run's next pass.
		s.logf("rundrive.heartbeat: lease for %s not renewable (fence %d) — death detection owns it",
			hc.workItemID, hc.fence)
	}
}

// releaseTerminal clears a terminal Run's checkout in one transaction:
// custody (holder/lease/run_id) released, §6.5 claim_released audit + outbox
// co-committed. The LANE returns to todo for failed and moves to the cancelled
// terminal for cancelled (ISI-4489), and stays in_progress for succeeded (M1.5's
// reporting owns completion lane moves) — guarded on in_progress, so items a
// human already moved are never disturbed.
func (s *HeartbeatSweeper) releaseTerminal(ctx context.Context, hc heldClaim) error {
	if hc.holderRun == "" {
		return nil // no run provenance to release under
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	if _, err := tx.ExecContext(ctx, `
		UPDATE coord.claim
		   SET holder_principal = NULL,
		       run_id           = NULL,
		       lease_expires_at = NULL
		 WHERE work_item_id = $1::uuid
		   AND run_id        = $2::uuid
		   AND reconcile_step IN ('succeeded','failed','cancelled')`,
		hc.workItemID, hc.holderRun); err != nil {
		return fmt.Errorf("release: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, fence_token, to_state)
		VALUES ($1::uuid, $2::uuid, 'claim_released', $3, $4, $5)`,
		hc.workItemID, hc.holderRun, hc.principal, hc.fence, string(hc.step)); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.outbox
		       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
		SELECT 'run', wi.project_id, wi.team_id::text, 'claim_released',
		       wi.id, $2::uuid,
		       jsonb_build_object('fence_token', $3::bigint, 'step', $4::text)
		  FROM coord.work_item wi WHERE wi.id = $1::uuid`,
		hc.workItemID, hc.holderRun, hc.fence, string(hc.step)); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}

	if hc.step != reconcile.StepSucceeded {
		// Failure/cancel moves the item off in_progress via the SAME
		// terminal-step → lane mapping the settle uses (coord.SettleLaneOf):
		// failed → todo (claimable again), cancelled → cancelled (terminal,
		// ISI-4489). Idempotent, guarded on in_progress — a human-moved lane is
		// never touched.
		if lane := coord.SettleLaneOf(string(hc.step)); lane != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE coord.work_item
				   SET state = $2, updated_at = now()
				 WHERE id = $1::uuid AND state = 'in_progress'`, hc.workItemID, lane); err != nil {
				return fmt.Errorf("lane return: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.logf("rundrive.heartbeat: released terminal claim %s (run %s, step %s)",
		hc.workItemID, hc.holderRun, hc.step)
	// ISI-7483: the run is terminal — its BYO endpoint permit (if any) must
	// not outlive the claim release (belt-and-braces to reclaimDeadHolders).
	s.releaseGateSlot(hc)
	return nil
}

// releaseOrphan clears a held claim whose backing Run CR no longer exists
// (ISI-5184): custody (holder/lease/run_id) released in one transaction with a
// claim_released audit row whose to_state records the orphan verdict. The
// reconcile_step of a dead run is untrustworthy provenance, so the LANE is
// deliberately NOT settled here — items whose machine state cannot be
// re-derived fall back to in_progress=false only via the guard below: the lane
// returns to todo (reclaimable) exactly as the terminal-failed path does,
// because a Run that vanished without a terminal commit is, from the machine's
// point of view, a failed holder. Human-moved lanes are never disturbed.
func (s *HeartbeatSweeper) releaseOrphan(ctx context.Context, hc heldClaim) error {
	return s.releaseDead(ctx, hc, "orphan_run_deleted")
}

// releaseZombie clears a held claim whose backing Run CR still EXISTS but whose
// sandbox pod is gone past the stall grace (ISI-5438). Identical custody release
// to releaseOrphan — the only difference is the audit/outbox reason, so the two
// kinds of dead holder are distinguishable in the §6.5 trail. The lane returns
// to todo (reclaimable); the wedged Run's reconcile_step is forced to failed so
// the projector terminalizes it and the 3.2 machine stops re-entering it.
func (s *HeartbeatSweeper) releaseZombie(ctx context.Context, hc heldClaim) error {
	return s.releaseDead(ctx, hc, "zombie_no_sandbox")
}

// releaseDead is the shared custody-release transaction for a dead holder
// (ISI-5184 orphan: Run CR gone; ISI-5438 zombie: Run CR present but pod gone).
// It nulls the custody triple, forces reconcile_step to failed, durably settles
// the dead a2a follow (ISI-7560 — else the projector's S3 hold pins the CR at
// Collecting), co-commits a §6.5 claim_released audit + outbox row carrying
// `reason`, and returns the lane to todo (guarded on in_progress so a human-moved
// lane is never touched). The release is idempotent: a re-sweep of an
// already-cleared row matches 0 rows and commits a no-op without re-auditing.
func (s *HeartbeatSweeper) releaseDead(ctx context.Context, hc heldClaim, reason string) error {
	if hc.holderRun == "" {
		return nil // no run provenance to release under
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	res, err := tx.ExecContext(ctx, `
		UPDATE coord.claim
		   SET holder_principal = NULL,
		       run_id           = NULL,
		       lease_expires_at = NULL,
		       reconcile_step   = 'failed'
		 WHERE work_item_id = $1::uuid
		   AND run_id        = $2::uuid
		   AND holder_principal IS NOT NULL`,
		hc.workItemID, hc.holderRun)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already cleared by a concurrent sweep / release — nothing to audit.
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, fence_token, to_state)
		VALUES ($1::uuid, $2::uuid, 'claim_released', $3, $4, $5)`,
		hc.workItemID, hc.holderRun, hc.principal, hc.fence, reason); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.outbox
		       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
		SELECT 'run', wi.project_id, wi.team_id::text, 'claim_released',
		       wi.id, $2::uuid,
		       jsonb_build_object('fence_token', $3::bigint, 'reason', $4::text)
		  FROM coord.work_item wi WHERE wi.id = $1::uuid`,
		hc.workItemID, hc.holderRun, hc.fence, reason); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}

	// ISI-7560: durably settle the dead follow. The holder died (e.g. the
	// operator rolled) before its follow goroutine could mark the a2a lap, so no
	// coord.a2a_dispatch row carries settled_at. Forcing reconcile_step='failed'
	// above is NOT enough to terminalize the Run CR: the status projector's S3
	// finalize-window hold (pkg/controller/run, ADR-0020 §2.4) re-derives the
	// phase from (dispatched && !settled) and pins it at Collecting/Running
	// forever — the observed orphan (Ready=Collecting on a claim that is terminal
	// failed, surviving every re-LIST because the projection is deterministically
	// wrong, not merely stale). Settle every unsettled lap of this run as
	// follow_error in the SAME release txn so the projector reads settled=true and
	// projects the terminal Failed phase without waiting for a full operator
	// re-LIST. Idempotent via the settled_at IS NULL guard: a re-sweep, or a lap a
	// racing settler (reattach / OnDone) already closed, matches nothing.
	if _, err := tx.ExecContext(ctx, `
		UPDATE coord.a2a_dispatch
		   SET settled_at = now(), settle_outcome = $2
		 WHERE run_id = $1::uuid AND settled_at IS NULL`,
		hc.holderRun, coord.SettleOutcomeFollowError); err != nil {
		return fmt.Errorf("settle dead follow: %w", err)
	}

	// The dead holder never committed a terminal step, so from the machine's
	// perspective it failed: the lane returns to todo (reclaimable), guarded on
	// in_progress so a human-moved lane is never touched.
	if _, err := tx.ExecContext(ctx, `
		UPDATE coord.work_item
		   SET state = $2, updated_at = now()
		 WHERE id = $1::uuid AND state = 'in_progress'`,
		hc.workItemID, "todo"); err != nil {
		return fmt.Errorf("lane return: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	// ISI-7483: the dead holder releases its BYO endpoint permit with its
	// custody — one leak starves every waiter on a single-slot endpoint.
	s.releaseGateSlot(hc)
	return nil
}

// isPausedStep reports whether a step is one of the intentional 3.7 park states
// (rate-limit AND all credential/endpoint variants — via the canonical PhaseOf
// projection, so a new paused variant is covered automatically). A paused Run's
// wait is deliberate, so the ISI-5438 zombie gate must never reap it.
func isPausedStep(s reconcile.Step) bool {
	return reconcile.PhaseOf(s) == reconcile.PhasePaused
}

// endpointSlotWaiter reports whether a pod-less in-flight claim must be EXEMPTED
// from the ISI-5438 zombie reap because its Run is legitimately queued behind a
// busy single-slot BYO endpoint (ISI-5591). The discussion/party-mode case fans
// N agents onto one endpoint whose slot concurrency is 1; agents routinely wait
// >10 min for the slot, and a waiter correctly boots NO sandbox pod until it
// holds one — so it trips the exact "pod-less past the stall grace" predicate.
// Reaping it (DEFECT A) force-fails a transient queued state into a TERMINAL
// Ready=Failed, which the intake driver then re-mints as a fresh generation
// (DEFECT B), resetting the queue position and churning r→r+1 every ~10 min.
// Exempting the waiter keeps its lease renewed and its FIFO position intact
// until the slot frees. Returns true (exempt) when the wait is confirmed AND,
// fail-safe, when the check errors — a possible waiter is never reaped on a
// maybe. Nil checker ⇒ false (pre-ISI-5591 behavior preserved for legacy wiring
// and tests).
func (s *HeartbeatSweeper) endpointSlotWaiter(ctx context.Context, hc heldClaim) bool {
	if s.EndpointWait == nil || hc.holderRun == "" {
		return false
	}
	waiting, err := s.EndpointWait.IsWaitingForEndpointSlot(ctx, hc.holderRun)
	if err != nil {
		s.logf("rundrive.heartbeat: endpoint-wait check for %s run %s: %v — exempting from zombie reap (fail-safe), renewing",
			hc.workItemID, hc.holderRun, err)
		return true
	}
	if waiting {
		s.logf("rundrive.heartbeat: run %s (item %s) queued on busy BYO endpoint slot, held %s — exempt from zombie reap, renewing lease to hold queue position",
			hc.holderRun, hc.workItemID, s.clock().Sub(hc.acquiredAt).Round(time.Second))
	}
	return waiting
}

// meshQueueWaiter reports whether a pod-less in-flight claim must be EXEMPTED
// from the zombie reap because its Run is queued at the Supervisor behind a
// saturated mesh pool (ISI-5831 S4, D9.1: QueuedAtSupervisor). The re-drive
// retry IS the bounded saturation queue (§5.3); the claim is renewed to hold
// the run's queue position, never released on the wait. Fails SAFE toward the
// waiter on a checker error (never reap on a maybe) — identical discipline to
// endpointSlotWaiter.
func (s *HeartbeatSweeper) meshQueueWaiter(ctx context.Context, hc heldClaim) bool {
	if s.MeshQueueWait == nil || hc.holderRun == "" {
		return false
	}
	waiting, err := s.MeshQueueWait.IsQueuedAtSupervisor(ctx, hc.holderRun)
	if err != nil {
		s.logf("rundrive.heartbeat: mesh-queue check for %s run %s: %v — exempting from zombie reap (fail-safe), renewing",
			hc.workItemID, hc.holderRun, err)
		return true
	}
	if waiting {
		s.logf("rundrive.heartbeat: run %s (item %s) queued at Supervisor on saturated mesh pool, held %s — exempt from zombie reap, renewing lease to hold queue position",
			hc.holderRun, hc.workItemID, s.clock().Sub(hc.acquiredAt).Round(time.Second))
	}
	return waiting
}

// agentSlotWaiter reports whether a pod-less in-flight claim must be
// EXEMPTED from the ISI-5438 zombie reap because its Run is legitimately
// queued on the per-agent non-preemptive dispatch queue (ISI-7499 C4,
// WaitingForAgentSlot). The run parks pod-less at the wait cadence until the
// agent's in-flight run goes terminal; reaping it would force-fail a
// transient queue state and churn r→r+1 re-mints, resetting the queue
// position. Fails SAFE toward the waiter on a checker error — identical
// discipline to endpointSlotWaiter/meshQueueWaiter.
func (s *HeartbeatSweeper) agentSlotWaiter(ctx context.Context, hc heldClaim) bool {
	if s.AgentWait == nil || hc.holderRun == "" {
		return false
	}
	waiting, err := s.AgentWait.IsWaitingForAgentSlot(ctx, hc.holderRun)
	if err != nil {
		s.logf("rundrive.heartbeat: agent-queue check for %s run %s: %v — exempting from zombie reap (fail-safe), renewing",
			hc.workItemID, hc.holderRun, err)
		return true
	}
	if waiting {
		s.logf("rundrive.heartbeat: run %s (item %s) queued on busy agent slot, held %s — exempt from zombie reap, renewing lease to hold queue position",
			hc.holderRun, hc.workItemID, s.clock().Sub(hc.acquiredAt).Round(time.Second))
	}
	return waiting
}

// clock is the sweep's time source (Now override, else time.Now).
func (s *HeartbeatSweeper) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// stalled reports whether a held claim has gone without progress long enough to
// qualify for the ISI-5438 zombie check: custody taken more than the stall grace
// ago. A row with no acquired_at anchor is never stalled (fail closed).
func (s *HeartbeatSweeper) stalled(hc heldClaim) bool {
	if hc.acquiredAt.IsZero() {
		return false
	}
	grace := s.ZombieStallGrace
	if grace <= 0 {
		grace = DefaultZombieStallGrace
	}
	return s.clock().Sub(hc.acquiredAt) > grace
}

func (s *HeartbeatSweeper) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}
