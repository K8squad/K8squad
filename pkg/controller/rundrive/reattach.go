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
	"fmt"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

// DefaultReattachWindow bounds which unsettled dispatch laps S4 re-attaches on
// leader-elect: dispatches older than this are assumed past the supervisor's
// session-retention window (ADR-0007) and unreachable. It is deliberately
// generous — larger than any expected agent run — so a genuinely long-running
// agent is re-attached rather than skipped; the cost of an over-long window is
// only a few Begin attempts that fail fast against gone supervisors.
const DefaultReattachWindow = 12 * time.Hour

// DefaultReattachFailTTL bounds how long a Submit-FAILING dispatch lap may keep
// coming back on leader-elects before the re-attach pass terminalizes it
// (ISI-7542). A lap whose re-attach Begin keeps failing is a corpse (dead
// sandbox supervisor): left unsettled, UnsettledDispatches returns it on EVERY
// leader-elect inside the re-attach window, so each operator restart re-Submits
// it (and re-grants its endpoint gates) forever — the ISI-7538 wedge. Once the
// lap is older than this TTL measured from dispatched_at, a failed Submit
// settles the lap follow_error instead: the row reaches a terminal settle
// state, the drive loop's settled-non-success death path (ISI-7524,
// ddf159d6) terminalizes the Run and releases its slots, and the S2 reaper —
// which only reaps settled runs — owns the leftover pod. The TTL must stay
// well under DefaultReattachWindow and comfortably above transient network
// blips (minutes): a genuinely long-running agent with a LIVE supervisor never
// hits it, because a live supervisor makes Submit succeed and the follow then
// settles the lap properly.
const DefaultReattachFailTTL = time.Hour

// reattachReader is the coord read side (coord.ProdReattachReader) that lists
// the unsettled dispatch laps eligible for follow re-attach.
type reattachReader interface {
	UnsettledDispatches(ctx context.Context, window time.Duration) ([]coord.ReattachTarget, error)
}

// followReattacher re-opens the background follow for one dispatch lap. In
// production this is *a2a.Dispatcher (Submit rebuilds the Task from
// (a2aTaskID, runID) and reattaches to the still-serving supervisor — C1
// idempotent, never a second agent execution). Submit spawning the follow
// goroutine is exactly the "re-open Client.Follow" of ADR-0020 §5(a): the
// dispatcher's OnDone then writes the S1 marker and releases the pod.
type followReattacher interface {
	Submit(ctx context.Context, a2aTaskID, runID string) error
}

// corpseSettler is the coord settle-write seam (coord.ProdSettler): it writes
// the durable, at-most-once follow-settlement marker that hands a re-attach
// corpse to the terminal machinery — UnsettledDispatches stops returning the
// lap (no more re-Submits), the drive loop's settled-non-success death path
// terminalizes the Run (ISI-7524), and the S2 reaper becomes willing to reap
// the pod (settled is its precondition). A nil Settler keeps the legacy
// best-effort behavior: failures are logged and the lap is left for the
// window to age out.
type corpseSettler interface {
	Settle(ctx context.Context, a2aTaskID, runID, outcome, reason string) error
}

// ReattachFollows is the ADR-0020 §5(a) leader-elect one-shot that closes the
// residual restart-during-work leak (ISI-4348-S4). On the elected leader it
// lists every unsettled, non-terminal dispatch lap and re-opens its follow, so
// an agent that finishes AFTER an operator restart is observed again: the
// re-opened follow drives to terminal, OnDone writes the durable S1 settlement
// marker, and the run-owned sandbox pod is released (live path) or reaped by S2
// (marker path) instead of leaking forever.
//
// It is registered via manager.Add, which defaults a plain Runnable to the
// leader-election group (one owner, after caches sync) — so it runs once per
// won leadership, which is precisely "on leader-elect".
type ReattachFollows struct {
	// Reader lists the laps to re-attach (required).
	Reader reattachReader
	// Dispatcher re-opens each lap's follow (required; the operator's A2A
	// dispatcher). A nil Dispatcher makes Start a no-op — the ledger-only
	// operator has no follows to re-attach.
	Dispatcher followReattacher
	// Settler terminalizes re-attach corpse laps once they outlive FailTTL
	// (optional; the operator's settle writer). Nil keeps the legacy
	// log-and-skip behavior (ISI-7542 callers that opt out).
	Settler corpseSettler
	// Window bounds re-attach eligibility by dispatch age; <= 0 uses
	// DefaultReattachWindow.
	Window time.Duration
	// FailTTL bounds how old a Submit-failing lap may be before it is settled
	// follow_error (measured from the lap's DispatchedAt); <= 0 uses
	// DefaultReattachFailTTL. Ignored when Settler is nil.
	FailTTL time.Duration
	// Log records the one-pass outcome and per-lap re-attach failures. Nil
	// discards.
	Log func(format string, args ...any)
}

// Start runs the one re-attach pass and returns. It is best-effort: a lap whose
// supervisor is already gone fails Submit (Begin cannot reach it), which is
// logged and skipped — that pod either settled via S2 already or is left for the
// TTL backstop (§5 option b), never blocking the remaining laps. A reader error
// aborts the pass but never the operator (returns nil): the next elected leader
// re-runs it.
func (r *ReattachFollows) Start(ctx context.Context) error {
	if r.Dispatcher == nil || r.Reader == nil {
		r.logf("a2a follow re-attach skipped: dispatcher or reader unset (ledger-only operator)")
		return nil
	}
	window := r.Window
	if window <= 0 {
		window = DefaultReattachWindow
	}
	failTTL := r.FailTTL
	if failTTL <= 0 {
		failTTL = DefaultReattachFailTTL
	}

	targets, err := r.Reader.UnsettledDispatches(ctx, window)
	if err != nil {
		r.logf("a2a follow re-attach: list unsettled dispatches failed (skipping this pass): %v", err)
		return nil
	}

	var reattached, failed, settled int
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			// Leadership lost / shutdown mid-pass: stop cleanly, the next leader re-runs.
			r.logf("a2a follow re-attach interrupted after %d/%d laps: %v", reattached+failed+settled, len(targets), err)
			return nil
		}
		if err := r.Dispatcher.Submit(ctx, t.A2ATaskID, t.RunID); err != nil {
			// Supervisor gone (Begin unreachable) or a transient submit error.
			// ISI-7542: a corpse lap older than FailTTL is terminalized via the
			// settle-write seam — otherwise UnsettledDispatches returns it on
			// every leader-elect and each restart re-Submits the corpse (and
			// re-grants its gates), the ISI-7538 wedge. Younger laps stay
			// unsettled: a transient failure may recover on a later pass.
			if s := r.settleCorpse(ctx, t, err, failTTL); s {
				settled++
				continue
			}
			failed++
			r.logf("a2a follow re-attach failed for lap %s (run %s); leaving to reaper: %v", t.A2ATaskID, t.RunID, err)
			continue
		}
		reattached++
	}
	r.logf("a2a follow re-attach done: reattached=%d failed=%d settled_corpses=%d candidates=%d window=%s fail_ttl=%s",
		reattached, failed, settled, len(targets), window, failTTL)
	return nil
}

// settleCorpse writes the durable follow_error settlement for one re-attach
// corpse lap — a lap whose Submit failed AND whose age (now - DispatchedAt, DB
// clock) is at least failTTL — and reports whether it did. It is the ISI-7542
// bounded-TTL ownership hand-off: settled_at IS NOT NULL removes the lap from
// UnsettledDispatches (never re-Submitted again), the settled non-success
// outcome routes the Run through the drive loop's death path (ISI-7524), and
// the S2 reaper — whose precondition is a settled run — reaps the leftover pod.
//
// follow_error (not failed) is the honest outcome: the supervisor was
// unreachable at Begin, which is a transport-level end with the agent's real
// outcome unknown — exactly the follow_error contract — and it avoids the
// failed-path ticket comment that would misattribute an engine failure to the
// agent. Settle is at-most-once (WHERE settled_at IS NULL), so a concurrent
// proper settlement wins cleanly; a Settle error is logged and the lap simply
// stays unsettled for the next pass (fail-open toward retry, never toward
// dropping a live lap). A zero DispatchedAt (reader that does not populate the
// dispatch instant) never settles — age cannot be proven, keep legacy behavior.
func (r *ReattachFollows) settleCorpse(ctx context.Context, t coord.ReattachTarget, submitErr error, failTTL time.Duration) bool {
	if r.Settler == nil || t.DispatchedAt.IsZero() {
		return false
	}
	if age := time.Since(t.DispatchedAt); age < failTTL {
		return false
	}
	reason := fmt.Sprintf("re-attach failed at lap age %s (>= corpse TTL): %v", time.Since(t.DispatchedAt).Round(time.Second), submitErr)
	if err := r.Settler.Settle(ctx, t.A2ATaskID, t.RunID, coord.SettleOutcomeFollowError, reason); err != nil {
		r.logf("a2a follow re-attach: settle corpse lap %s (run %s) failed (stays unsettled for next pass): %v", t.A2ATaskID, t.RunID, err)
		return false
	}
	r.logf("a2a follow re-attach: corpse lap %s (run %s) settled follow_error after %s (>= TTL %s); death path terminalizes the run, S2 reaps the pod: %v",
		t.A2ATaskID, t.RunID, time.Since(t.DispatchedAt).Round(time.Second), failTTL, submitErr)
	return true
}

func (r *ReattachFollows) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}
