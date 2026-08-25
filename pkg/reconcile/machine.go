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

// Package reconcile is the crash-safe Run reconcile state machine — the heart of
// I1 (arch §8, §6.4, ADR-005; Story 3.1 / ISI-2535). It is the shipping-language
// embodiment of the design proved by the differential crash-injection
// falsification docs/bmad/spikes/bench/run-reconcile-check.py.
//
// The load-bearing invariant is crash-safe idempotency: a controller can die at
// ANY phase boundary and the failover leader re-reads the DURABLE reconcile_step
// (the Postgres source of truth, never controller memory) and continues — with
// each phase's external side effect happening AT MOST ONCE across all crash
// windows. The machine keeps ZERO liveness/continuity state in memory: every
// reconcile pass is level-triggered from the committed step alone (AC1/AC2).
//
// # The two-level model (arch §5.1 r28 vs §8)
//
// The CRD-visible Run.status.phase enum is the coarse, CEL-validated set pinned
// at architecture r28: Pending | Claiming | Running | Paused | Succeeded | Failed
// | Cancelled. The originating issue's finer ClaimingSandbox / Dispatching /
// Collecting are NOT new enum values — they are durable reconcile_step
// checkpoints (the Postgres re-entry truth) that project onto the coarse phase
// via PhaseOf. status.phase is the projection; the durable Step is the truth
// (AC2). See the story's checkpoint→phase table.
//
// # Per-phase idempotency (the §6.4 crux, AC3)
//
//   - claiming_sandbox — warm-pool bind keyed by run_id: a re-entry (mid-phase
//     crash OR a Failed→Claiming retry lap) REATTACHES, never double-provisions.
//   - dispatching      — deterministic a2a_task_id = run_id (per retry lap) + shim
//     dedup: no crash window yields two agent executions.
//   - collecting       — content-addressed artifact upsert: a re-entry republishes
//     the identical row, never a duplicate.
//   - every transition — a conditional step-advance (Advance guarded by
//     WHERE reconcile_step = :expected AND fence_token = :fence): a stale/duplicate
//     pass can neither double-advance nor resurrect a Run.
//
// # Two split-brain shapes, two safety mechanisms (AC4)
//
// Leader-election is AVAILABILITY; fencing is SAFETY. A zombie whose fence was
// bumped out by a §6.3 fence-first reclaim loses the fence-EQUALITY guard
// (fencing covers the reclaim zombie). Two leaders that share the SAME fence
// (claim never reclaimed) are indistinguishable to fencing — there the step-CAS
// (WHERE reconcile_step = :expected) serializes them to exactly one advance.
//
// # Scope
//
// This package ships the state machine, its durable checkpointing, per-phase
// idempotency, and the leader-election + fencing/step-CAS failover LOGIC over a
// small Store/Effects seam, plus the crash-injection falsification in the
// shipping language (machine_test.go). The production wiring of that seam — the
// coord Postgres reconcile_step schema, the warm-pool sandbox bind (Story 3.2),
// the CRD status patch, and leader election in cmd/main.go — is the integration
// layer that plugs into this machine; the real-Postgres integration test remains
// the Story 2.7 gate.
package reconcile

import "fmt"

// Phase is the coarse, CRD-visible Run.status.phase enum (arch §5.1 r28,
// CEL-validated). It is a PROJECTION of the durable Step (AC2), never the
// reconcile source of truth.
type Phase string

const (
	PhasePending   Phase = "Pending"
	PhaseClaiming  Phase = "Claiming"
	PhaseRunning   Phase = "Running"
	PhasePaused    Phase = "Paused"
	// PhaseCanceling is the 3.3 operator-kill transitional phase (spec
	// "Canceling"): teardown owed, terminal Cancelled pending.
	PhaseCanceling Phase = "Canceling"
	PhaseSucceeded Phase = "Succeeded"
	PhaseFailed    Phase = "Failed"
	PhaseCancelled Phase = "Cancelled"
)

// Step is the fine-grained durable reconcile_step — the Postgres source of truth
// the machine re-enters from after a crash/failover (arch §6.4). The happy path
// is pending → claiming_sandbox → dispatching → running → collecting → succeeded;
// the terminal set and Paused are off the linear path.
type Step string

const (
	StepPending           Step = "pending"
	StepClaimingSandbox   Step = "claiming_sandbox"
	StepDispatching       Step = "dispatching"
	StepRunning           Step = "running"
	StepCollecting        Step = "collecting"
	StepSucceeded         Step = "succeeded"
	StepFailed            Step = "failed"
	StepCancelled         Step = "cancelled"
	// StepCancelling is the 3.3 operator-kill transitional step: kill was
	// issued (fence-first CancelEnter), the driver owes the sandbox teardown
	// and the guarded finish → cancelled. Resumable (not terminal): a driver
	// crash mid-teardown re-enters it and finishes the job (AC5).
	StepCancelling        Step = "cancelling"
	StepPaused            Step = "paused"
	StepPausedRateLimited Step = "paused(rate_limited)"
	// StepPausedCredentialExpired is the 7.4 credential-expiry hold (arch §10,
	// FR-G3): the shim's credentialLifecycle surfaced an expired credential
	// (OAuth refresh window lapsed, static key revoked). NOT timer-resumable —
	// it clears only on refresh/rotation write-back (ResumeOnRefresh), which is
	// what makes it a legible Paused instead of an opaque auth failure.
	StepPausedCredentialExpired Step = "paused(credential_expired)"
	// StepPausedCredentialRotated is the 7.4 rotation hold: the credential was
	// rotated mid-Run (Secret update observed under the shim's feet). The
	// in-flight agent context is stale; the Run pauses until the new credential
	// material propagates (ResumeOnRefresh), then re-enters Running.
	StepPausedCredentialRotated Step = "paused(credential_rotated)"
	// StepPausedEndpointUnreachable is the 7.4/7.5 BYO-endpoint hold: the
	// Ollama/endpoint-URL credential model's endpoint is unreachable — a
	// legible Paused in the same family, never an opaque dial failure.
	StepPausedEndpointUnreachable Step = "paused(endpoint_unreachable)"
)

// CredentialPauseSteps is the closed family of credential-driven pause steps
// (Story 7.4/7.6): one pause/resume machinery, distinct legible reasons. Every
// member projects onto PhasePaused and classifies resumable; members differ
// ONLY in why the Run is held and what resumes it (timer vs refresh).
var CredentialPauseSteps = map[Step]bool{
	StepPausedRateLimited:         true, // 7.6: per-credential attribution, Retry-After timer resume
	StepPausedCredentialExpired:   true, // 7.4: OAuth/static-key expiry, refresh resume
	StepPausedCredentialRotated:   true, // 7.4: rotation, propagation resume
	StepPausedEndpointUnreachable: true, // 7.4/7.5: BYO endpoint, recovery resume
}

// happyPath is the linear reconcile progression the machine advances through
// (arch §8). Terminal steps and Paused are reached off this path (fail_at edge,
// rate-limit tiers) and are handled by the classifier, not this slice.
var happyPath = []Step{
	StepPending, StepClaimingSandbox, StepDispatching,
	StepRunning, StepCollecting, StepSucceeded,
}

// terminalSteps are absorbing (AC5): no further reconcile advances them. Only
// Failed + retryPolicy-within-budget re-enters Claiming (via an explicit
// SetStep after a §6.3 fence-first reclaim); that is a caller decision, not an
// automatic edge of this slice.
var terminalSteps = map[Step]bool{
	StepSucceeded: true,
	StepFailed:    true,
	StepCancelled: true,
}

// resumableSteps are the non-terminal steps a reconcile loop may re-enter after
// a crash/failover (AC5). Paused / Paused(rate_limited) / the credential pause
// family are included so the machine does NOT make them unreachable — driving
// them is §8's rate-limit tiers and §10's credential lifecycle (Story 7.4/7.6),
// but a classifier that dropped them into "unknown" would let a future story
// wire a phase this machine can never re-enter.
var resumableSteps = map[Step]bool{
	StepPending:                   true,
	StepClaimingSandbox:           true,
	StepDispatching:               true,
	StepRunning:                   true,
	StepCollecting:                true,
	StepCancelling:                true,
	StepPaused:                    true,
	StepPausedRateLimited:         true,
	StepPausedCredentialExpired:   true,
	StepPausedCredentialRotated:   true,
	StepPausedEndpointUnreachable: true,
}

// Classification is the AC5 terminal-vs-resumable verdict for a step.
type Classification string

const (
	ClassTerminal  Classification = "terminal"
	ClassResumable Classification = "resumable"
	// ClassUnknown is a step the machine can neither absorb nor resume — an AC5
	// gap. A well-formed machine never produces one.
	ClassUnknown Classification = "unknown"
)

// Classify reports whether a step is terminal (absorbing) or non-terminal
// (resumable) — AC5. Paused/Paused(rate_limited) and the credential pause
// family (7.4/7.6) classify resumable so the rate-limit-recovery and
// credential-refresh stories wire them without re-architecting the machine.
func Classify(s Step) Classification {
	if terminalSteps[s] {
		return ClassTerminal
	}
	if resumableSteps[s] {
		return ClassResumable
	}
	return ClassUnknown
}

// IsTerminal reports whether a durable step is absorbing (AC5).
func IsTerminal(s Step) bool { return terminalSteps[s] }

// PhaseOf projects a durable reconcile_step onto the coarse CRD Run.status.phase
// enum (arch §5.1 r28) — status is downstream of the durable step (AC2). The
// ClaimingSandbox/Dispatching checkpoints both surface as Claiming; running and
// collecting both surface as Running (see the story checkpoint→phase table).
func PhaseOf(s Step) Phase {
	switch s {
	case StepPending:
		return PhasePending
	case StepClaimingSandbox, StepDispatching:
		return PhaseClaiming
	case StepRunning, StepCollecting:
		return PhaseRunning
	case StepPaused, StepPausedRateLimited, StepPausedCredentialExpired,
		StepPausedCredentialRotated, StepPausedEndpointUnreachable:
		return PhasePaused
	case StepCancelling:
		return PhaseCanceling
	case StepSucceeded:
		return PhaseSucceeded
	case StepFailed:
		return PhaseFailed
	case StepCancelled:
		return PhaseCancelled
	default:
		return ""
	}
}

// Store is the durable coordination seam (arch §6.4) — the reconcile_step + fence
// that are the Run's source of truth. The production implementation is a coord
// Postgres binding whose Advance is a single conditional UPDATE co-committing the
// §6.5 audit row and §6.6 outbox event; MemStore is the in-memory model the
// falsification drives.
type Store interface {
	// Step returns the committed durable reconcile_step.
	Step() Step
	// Fence returns the current monotonic fence token (arch §6.3).
	Fence() int64
	// Advance is the conditional step-advance:
	//   UPDATE ... SET reconcile_step = :next
	//   WHERE reconcile_step = :expected AND fence_token = :fence
	// co-committing an audit row (§6.5) + outbox event (§6.6) in the SAME
	// transaction (AC6). It returns true iff it committed. A nil fence models the
	// NAIVE reconciler that takes no fence guard at all. Two independent guards:
	// the fence-EQUALITY guard covers the reclaim (bumped-fence) zombie; the
	// reconcile_step = :expected step-CAS covers the same-fence competitor (AC4).
	Advance(expected, next Step, fence *int64) bool
	// Reclaim is the §6.3 fence-first reclaim: fence the dead pod (bump the
	// token) BEFORE the failover leader re-enters. Monotonic — returns false if
	// newFence would not raise the token.
	Reclaim(newFence int64) bool
	// SetStep re-points the durable step without a guard — used only for the §8
	// Failed→Claiming retry re-entry AFTER a fence-first reclaim (a caller
	// decision bounded by retryPolicy), never on the happy path.
	SetStep(Step)
	// AuditRows / OutboxRows expose the co-committed §6.5/§6.6 counts so the
	// falsification can assert exactly one of each per committed transition (AC6).
	AuditRows() int
	OutboxRows() int
}

// Effects is the side-effecting outside world — the things that must happen at
// most once (arch §6.4). Each method's boolean selects the DURABLE idempotency
// mechanism (true) vs the NAIVE unguarded behaviour (false); the falsification
// flips them to prove each mechanism is load-bearing.
type Effects interface {
	// BindSandbox provisions/attaches the warm-pool sandbox for runID
	// (claiming_sandbox, Story 3.2). keyed=true reattaches to an existing bind
	// for runID (never double-provisions); keyed=false provisions afresh.
	BindSandbox(runID string, keyed bool)
	// Dispatch submits the A2A task to the shim (dispatching, §6.4/§10.1).
	// dedup=true makes the shim reattach to an in-flight taskID (no second agent
	// execution); dedup=false always starts a new execution.
	Dispatch(taskID string, dedup bool)
	// Collect registers an artifact (collecting, §6.1). upsert=true republishes
	// the same content-addressed key idempotently; upsert=false appends a dupe.
	Collect(key, content string, upsert bool)
	// Terminal records a terminal transition (succeeded/failed/cancelled).
	Terminal(s Step)
}

// RunID is the deterministic run identifier the idempotency mechanisms key on
// (a2a_task_id = run_id, sandbox bind key, artifact namespace). The falsification
// uses a single fixed run; production threads the real Run's uid here.
const RunID = "run-1"

// Options parameterises one reconcile drive. The Crash* fields inject a crash at
// a specific window (empty = no crash); Lap/FailAt drive the §8 Failed→Claiming
// retry lap. Durable selects the §6.4 machine vs the naive in-memory reconciler.
type Options struct {
	// Durable selects the §6.4 durable machine (true) or the naive in-memory
	// reconciler (false) that resumes from controller memory and double-drives.
	Durable bool
	// Fence is the leader's current fence token, passed to Advance's equality
	// guard. Ignored when Durable is false (the naive reconciler passes no fence).
	Fence int64
	// CrashBefore injects a crash at a phase BOUNDARY (before the effect). "" =
	// none. Resumes trivially from the cleanly-committed prior step.
	CrashBefore Step
	// CrashAfterEffect injects a crash at the §6.4 CRUX window: the external
	// effect has fired but the step-advance transaction has NOT committed. "" =
	// none. This is the only window that forces an idempotent re-drive on
	// failover, so it is the only one that exercises the dedup/upsert/bind guards.
	CrashAfterEffect Step
	// Lap disambiguates the dispatch task id across retry attempts: deterministic
	// WITHIN a lap (a crash re-drive dedups), fresh execution per lap (a retry
	// re-dispatches). nil = the single-attempt happy path (task id = run_id).
	Lap *int
	// FailAt diverts the given step to the terminal Failed edge (models an agent
	// attempt failing), driving the §8/FR-A5 retry lap. "" = no failure.
	FailAt Step
	// MaxPasses bounds the per-phase loop iterations of ONE Reconcile call (0 =
	// unlimited). It is not part of the falsification surface: with a healthy
	// store every Advance commits and the loop terminates on a terminal step.
	// The PRODUCTION driver sets it because a pass whose expected step or fence
	// no longer holds (a concurrent §6.3 reclaim raced the drive) makes Advance
	// commit nothing while Step() keeps returning the same step — an unbounded
	// spin. Exhausting the bound returns nil with the step non-terminal; the
	// level-triggered caller re-reads the (new) fence/step and requeues.
	MaxPasses int
}

// ErrCrash is returned by Reconcile when a crash was injected at a configured
// window — the controller process is considered dead at that point. Go has no
// exceptions; this sentinel models the Python falsification's Crash.
type ErrCrash struct{ At Step }

func (e ErrCrash) Error() string { return fmt.Sprintf("controller crash at step %q", e.At) }

// Reconcile drives the durable machine from its current committed step toward a
// terminal step, or returns ErrCrash if a crash was injected. It is
// LEVEL-TRIGGERED: every pass reads store.Step() fresh and acts on it, never
// relying on the previous pass having run in the same process (AC1). The durable
// step is the only recovery state — a fresh process reconstructs exactly where
// the Run is from it alone (AC2). A non-nil MaxPasses bounds the loop for
// production drivers (see Options.MaxPasses) without altering the machine's
// semantics: exhaustion is a non-terminal return, never a synthetic state.
func Reconcile(w Effects, s Store, o Options) error {
	passes := 0
	for !IsTerminal(s.Step()) {
		if o.MaxPasses > 0 && passes >= o.MaxPasses {
			return nil // bounded stop: step is non-terminal; caller requeues
		}
		passes++
		step := s.Step()
		if o.CrashBefore != "" && step == o.CrashBefore {
			return ErrCrash{At: step}
		}
		if err := runPhase(step, w, s, o); err != nil {
			return err
		}
	}
	return nil
}

// runPhase performs one step's external effect (idempotently, §6.4) and then
// commits the durable step-advance with its co-committed audit + outbox rows
// (AC6). The effect fires BEFORE the advance; a CrashAfterEffect between them is
// the crux window a failover must re-drive idempotently.
func runPhase(step Step, w Effects, s Store, o Options) error {
	switch step {
	case StepClaimingSandbox:
		// §6.2/§9 warm-pool bind, keyed by run_id — re-entry (crash OR retry lap)
		// reattaches to the sandbox already provisioned for this run.
		w.BindSandbox(RunID, o.Durable)
	case StepDispatching:
		// DURABLE: deterministic task id = run_id (per lap), shim dedups.
		// NAIVE: a fresh execution every pass (no dedup) → double-drive.
		w.Dispatch(dispatchTaskID(o), o.Durable)
	case StepCollecting:
		// Content-addressed upsert (§6.1): a re-entry republishes the same row.
		w.Collect(RunID+"/patch", "diff-bytes", o.Durable)
	}

	// §6.4 crux window: effect applied, step-advance NOT yet committed.
	if o.CrashAfterEffect != "" && step == o.CrashAfterEffect {
		return ErrCrash{At: step}
	}

	idx := indexOf(step)
	if idx < 0 || idx+1 >= len(happyPath) {
		return nil // off-path or already at the terminal happy-path step
	}
	next := happyPath[idx+1]
	if o.FailAt != "" && step == o.FailAt {
		next = StepFailed // §8/FR-A5 divert to the Failed edge
	}
	// The conditional step-advance is the serialization point (AC3/AC4). A stale
	// or zombie pass whose expected/fence no longer matches commits nothing.
	fence := &o.Fence
	if !o.Durable {
		fence = nil // the naive reconciler takes no fence guard
	}
	if s.Advance(step, next, fence) && IsTerminal(next) {
		w.Terminal(next)
	}
	return nil
}

// dispatchTaskID computes the A2A task id for the dispatching step. DURABLE ids
// are deterministic within a lap (so a crash re-drive dedups) but distinct across
// retry laps (so a retry is a genuine new agent attempt). The naive branch is
// unused for id purposes (its Dispatch passes dedup=false and always appends).
func dispatchTaskID(o Options) string {
	if !o.Durable {
		return RunID + "-naive"
	}
	if o.Lap == nil {
		return RunID
	}
	return fmt.Sprintf("%s#lap%d", RunID, *o.Lap)
}

func indexOf(step Step) int {
	for i, s := range happyPath {
		if s == step {
			return i
		}
	}
	return -1
}
