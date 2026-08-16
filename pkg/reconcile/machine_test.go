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

// Crash-safe reconcile falsification, in the shipping language (Story 3.1 /
// ISI-2535). This is the Go counterpart of the approved differential spike
// docs/bmad/spikes/bench/run-reconcile-check.py: the same crash-injection
// scenario run two ways —
//
//	(A) a NAIVE reconciler that keeps continuity in controller memory DOES lose
//	    progress / double-drive across failover (proves the harness has teeth);
//	(B) the §6.4 DURABLE machine produces exactly-once bind/dispatch/collect/
//	    terminal, zero lost progress, for a crash at EVERY window.
//
// If the naive arm ever stops breaking, the durable proof is meaningless and
// the harness has lost its detecting power — TestNaiveDetectablyBreaks fails loud.
package reconcile

import (
	"reflect"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// happyBoundaries is the crash-boundary set (every non-terminal happy-path step).
func happyBoundaries() []Step { return happyPath[:len(happyPath)-1] }

// driveNaive models a naive reconciler process: continuity lives in controller
// memory, so a replaced (failed-over) process re-reads NOTHING durable and
// restarts from `from`. Each call is a fresh process against the shared World.
func driveNaive(w *World, from, crashBefore Step) {
	s := NewMemStore()
	s.SetStep(from)
	_ = Reconcile(w, s, Options{Durable: false, CrashBefore: crashBefore})
}

// (A) The naive design MUST break under failover, or the harness has no teeth.
func TestNaiveDetectablyBreaks(t *testing.T) {
	broke := false
	// Crash AFTER the dispatch effect has fired (running/collecting boundaries),
	// so leader 1 already started an agent execution when it dies.
	for _, crashAt := range []Step{StepRunning, StepCollecting} {
		w := NewWorld()
		driveNaive(w, StepPending, crashAt) // leader 1 dies at crashAt
		driveNaive(w, StepPending, "")      // failover restarts from pending (memory gone)
		if len(w.AgentExecutions) > 1 || len(w.Artifacts) > 1 {
			broke = true
		}
	}
	if !broke {
		t.Fatal("NAIVE reconciler did NOT double-drive across failover — the falsification " +
			"lost its detecting power; the durable proof is meaningless. FIX THE HARNESS.")
	}
}

// (B) §6.4: inject a crash at EVERY phase boundary; assert exactly-once effects
// after a §6.3 fence-first reclaim + failover, and that a stale-fence zombie loses
// and writes NO audit/outbox row (AC2/AC3/AC4/AC6).
func TestDurableCrashAtEveryBoundary(t *testing.T) {
	for _, crashAt := range happyBoundaries() {
		w := NewWorld()
		s := NewMemStore()

		// Leader 1 runs until it crashes at the boundary.
		if err := Reconcile(w, s, Options{Durable: true, Fence: 1, CrashBefore: crashAt}); err == nil {
			t.Fatalf("crash@%s: expected a crash, machine ran to completion", crashAt)
		}

		// A zombie old leader issues a STALE-fence mutation (fence 0 < current 1):
		// must lose, and (AC6) must write NEITHER an audit row NOR an outbox event.
		auditBefore, outboxBefore := s.AuditRows(), s.OutboxRows()
		if s.Advance(s.Step(), StepSucceeded, ptr[int64](0)) {
			t.Fatalf("crash@%s: zombie stale-fence write WON — fencing broken", crashAt)
		}
		if s.AuditRows() != auditBefore || s.OutboxRows() != outboxBefore {
			t.Fatalf("crash@%s: fenced-out zombie wrote a phantom audit/event (AC6 broken)", crashAt)
		}

		// Failover: §6.3 fence-first reclaim bumps the token, THEN re-read + continue.
		if !s.Reclaim(2) {
			t.Fatalf("crash@%s: failover reclaim failed to bump the fence", crashAt)
		}
		if err := Reconcile(w, s, Options{Durable: true, Fence: 2}); err != nil {
			t.Fatalf("crash@%s: failover reconcile errored: %v", crashAt, err)
		}

		assertExactlyOnce(t, "crash@"+string(crashAt), w, s)
	}
}

// (C) The §6.4 CRUX: crash AFTER an effect fires but BEFORE its step commits —
// the only window that re-drives an already-performed effect, so the only one
// that actually exercises the run_id-keyed bind / dedup / upsert (AC3).
func TestDurableIntraphaseCrux(t *testing.T) {
	for _, crashAt := range []Step{StepClaimingSandbox, StepDispatching, StepCollecting} {
		w := NewWorld()
		s := NewMemStore()

		// Leader 1 runs until the effect at crashAt has fired, then dies pre-commit.
		if err := Reconcile(w, s, Options{Durable: true, Fence: 1, CrashAfterEffect: crashAt}); err == nil {
			t.Fatalf("intraphase@%s: expected a crash, machine ran to completion", crashAt)
		}
		// The durable step is still the SAME phase — effect happened, advance did not.
		if s.Step() != crashAt {
			t.Fatalf("intraphase@%s: step advanced to %s before commit — effect and advance not atomic",
				crashAt, s.Step())
		}
		// Failover re-enters the SAME phase and MUST re-drive it idempotently.
		if !s.Reclaim(2) {
			t.Fatalf("intraphase@%s: failover reclaim failed to bump the fence", crashAt)
		}
		if err := Reconcile(w, s, Options{Durable: true, Fence: 2}); err != nil {
			t.Fatalf("intraphase@%s: failover reconcile errored: %v", crashAt, err)
		}

		assertExactlyOnce(t, "intraphase@"+string(crashAt), w, s)
	}
}

// (D) F4/AC4: on a same-fence split-brain (claim NOT reclaimed) fencing cannot
// distinguish two equal-fence leaders — the step-CAS (WHERE reconcile_step =
// :expected), NOT fencing, serializes them to exactly one advance.
func TestSameFenceSplitBrain(t *testing.T) {
	w := NewWorld()
	s := NewMemStore()
	if !s.Advance(StepPending, StepClaimingSandbox, ptr[int64](1)) {
		t.Fatal("setup: could not reach a mid-machine step")
	}
	step := s.Step()
	auditBefore, outboxBefore := s.AuditRows(), s.OutboxRows()

	// No reclaim → both leaders hold fence 1. Fencing is a no-op discriminator here.
	aOK := s.Advance(step, StepDispatching, ptr[int64](1))
	bOK := s.Advance(step, StepDispatching, ptr[int64](1)) // SAME fence — must lose on the step-CAS
	if !aOK || bOK {
		t.Fatalf("same-fence split-brain: aOK=%v bOK=%v — step-CAS failed to serialize equal-fence writers", aOK, bOK)
	}
	if s.Step() != StepDispatching {
		t.Fatalf("same-fence: step is %s (want dispatching) — did not advance exactly once", s.Step())
	}
	if s.AuditRows() != auditBefore+1 || s.OutboxRows() != outboxBefore+1 {
		t.Fatal("same-fence split-brain: a second audit/outbox row leaked past the step-CAS (double-advance)")
	}
	_ = w
}

// (E) F3/AC5 + §8/FR-A5: the Failed → Claiming retry lap — the trickiest re-entry.
// Lap 1 fails; a §6.3 fence-first reclaim re-enters claiming_sandbox; the retry
// re-runs it IDEMPOTENTLY (run_id-keyed bind reattaches, provisioned once), but
// each lap is a genuine new agent attempt (a retry re-dispatches).
func TestFailedToClaimingRetryLap(t *testing.T) {
	w := NewWorld()
	s := NewMemStore()

	// Lap 1: pending → claiming_sandbox (bind) → dispatching (lap0) → running → FAILED.
	if err := Reconcile(w, s, Options{Durable: true, Fence: 1, Lap: ptr(0), FailAt: StepRunning}); err != nil {
		t.Fatalf("retry lap 1 errored: %v", err)
	}
	if s.Step() != StepFailed {
		t.Fatalf("retry lap 1 did not reach failed: %s", s.Step())
	}
	if !reflect.DeepEqual(w.SandboxBinds, []string{RunID}) {
		t.Fatalf("lap 1 sandbox bind not once: %v", w.SandboxBinds)
	}
	if !reflect.DeepEqual(w.AgentExecutions, []string{RunID + "#lap0"}) {
		t.Fatalf("lap 1 dispatch wrong: %v", w.AgentExecutions)
	}

	// retryPolicy within budget: §6.3 fence-first reclaim, THEN re-enter Claiming.
	if !s.Reclaim(2) {
		t.Fatal("retry: fence-first reclaim did not bump the fence")
	}
	s.SetStep(StepClaimingSandbox) // §8 Failed → Claiming after the reclaim

	// Lap 2: the retry re-runs claiming_sandbox; the run_id-keyed bind MUST reattach.
	if err := Reconcile(w, s, Options{Durable: true, Fence: 2, Lap: ptr(1)}); err != nil {
		t.Fatalf("retry lap 2 errored: %v", err)
	}
	if s.Step() != StepSucceeded {
		t.Fatalf("retry lap 2 did not succeed: %s", s.Step())
	}
	// Load-bearing: claiming_sandbox ran on BOTH laps, provisioned EXACTLY once.
	if !reflect.DeepEqual(w.SandboxBinds, []string{RunID}) {
		t.Fatalf("retry re-provisioned the sandbox: %v — bind not idempotent across the Failed→Claiming lap", w.SandboxBinds)
	}
	// A retry is a genuine new attempt: one distinct execution per lap, no cross-lap dedup.
	if !reflect.DeepEqual(w.AgentExecutions, []string{RunID + "#lap0", RunID + "#lap1"}) {
		t.Fatalf("retry agent attempts wrong: %v — a retry lap must re-dispatch a fresh execution", w.AgentExecutions)
	}
}

// (F) F3/AC5: Paused + Paused(rate_limited) are non-terminal RESUMABLE, not
// terminal — the machine must not make them unreachable for a later rate-limit
// story. Succeeded/Failed/Cancelled are terminal (absorbing).
func TestPausedClassifierReachable(t *testing.T) {
	for _, p := range []Step{StepPaused, StepPausedRateLimited} {
		if IsTerminal(p) {
			t.Fatalf("AC5: %s classified terminal — it is non-terminal resumable", p)
		}
		if Classify(p) != ClassResumable {
			t.Fatalf("AC5: %s is %s (want resumable) — the machine made Paused unreachable", p, Classify(p))
		}
	}
	for _, p := range []Step{StepSucceeded, StepFailed, StepCancelled} {
		if Classify(p) != ClassTerminal {
			t.Fatalf("AC5: %s must be terminal (absorbing), got %s", p, Classify(p))
		}
	}
}

// AC1/AC2: status.phase is a PROJECTION of the durable reconcile_step — the fine
// checkpoints ClaimingSandbox/Dispatching both surface as the coarse Claiming
// phase; running/collecting both surface as Running.
func TestPhaseProjection(t *testing.T) {
	cases := map[Step]Phase{
		StepPending:           PhasePending,
		StepClaimingSandbox:   PhaseClaiming,
		StepDispatching:       PhaseClaiming,
		StepRunning:           PhaseRunning,
		StepCollecting:        PhaseRunning,
		StepPaused:            PhasePaused,
		StepPausedRateLimited: PhasePaused,
		StepSucceeded:         PhaseSucceeded,
		StepFailed:            PhaseFailed,
		StepCancelled:         PhaseCancelled,
	}
	for step, want := range cases {
		if got := PhaseOf(step); got != want {
			t.Fatalf("PhaseOf(%s) = %s, want %s", step, got, want)
		}
	}
}

// assertExactlyOnce is the shared post-failover invariant: the Run reached
// succeeded with each external effect having happened exactly once, zero lost
// progress, and one audit + one outbox row per committed transition (AC2/AC3/AC6).
func assertExactlyOnce(t *testing.T, ctx string, w *World, s *MemStore) {
	t.Helper()
	if s.Step() != StepSucceeded {
		t.Fatalf("%s: not terminal (step=%s)", ctx, s.Step())
	}
	if !reflect.DeepEqual(w.SandboxBinds, []string{RunID}) {
		t.Fatalf("%s: sandbox provisioned %v (want once) — bind not run_id-keyed across failover", ctx, w.SandboxBinds)
	}
	if len(w.AgentExecutions) != 1 {
		t.Fatalf("%s: %d agent executions (want 1) — double-dispatch across failover", ctx, len(w.AgentExecutions))
	}
	if !reflect.DeepEqual(w.Artifacts, map[string]string{RunID + "/patch": "diff-bytes"}) {
		t.Fatalf("%s: duplicate/lost artifact %v", ctx, w.Artifacts)
	}
	if !reflect.DeepEqual(w.TerminalTransitions, []Step{StepSucceeded}) {
		t.Fatalf("%s: %v terminal transitions (want [succeeded])", ctx, w.TerminalTransitions)
	}
	// AC6: exactly one audit row + one outbox event per committed transition — no
	// double-audit across failover, no transition with no trail.
	wantRows := len(happyPath) - 1
	if s.AuditRows() != wantRows || s.OutboxRows() != wantRows {
		t.Fatalf("%s: %d audit / %d outbox rows (want %d each) — audit/event not co-committed 1:1 with transitions",
			ctx, s.AuditRows(), s.OutboxRows(), wantRows)
	}
}
