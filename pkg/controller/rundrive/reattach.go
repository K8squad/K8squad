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

// DefaultReattachWindow bounds which unsettled dispatches S4 re-attaches on
// leader-elect: dispatches older than this are assumed past the supervisor's
// session-retention window (ADR-0007) and unreachable. It is deliberately
// generous — larger than any expected agent run — so a genuinely long-running
// agent is re-attached rather than skipped; the cost of an over-long window is
// only a few Begin attempts that fail fast against gone supervisors.
const DefaultReattachWindow = 12 * time.Hour

// DefaultReattachFailTTL bounds how long a Submit-failing unsettled dispatch
// lap is retried on leader-elects before the reattach pass terminally settles
// it (follow_error) and the reaper owns it (ISI-7540, out of the ISI-7538
// repro-3 corpse wedge: a dead-sandbox lap otherwise re-Submits on EVERY elect
// for the whole DefaultReattachWindow, re-granting gates for a run that can
// never settle). It must sit comfortably above any transient Submit failure a
// healthy lap rides out between two elects, and well below
// DefaultReattachWindow so the corpse reaches a terminal settle state in a
// bounded window.
const DefaultReattachFailTTL = 2 * time.Hour

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

// reattachSettler is the coord settle-write seam (coord.ProdSettler in
// production). Settle records the at-most-once terminal marker
// (settled_at/settle_outcome + 'a2a_settled' audit row) that makes
// UnsettledDispatches stop returning the lap on later leader-elects.
type reattachSettler interface {
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
	// Window bounds re-attach eligibility by dispatch age; <= 0 uses
	// DefaultReattachWindow.
	Window time.Duration
	// Settler terminally settles a re-attach-failing corpse lap once it is
	// older than FailTTL (optional; nil keeps the legacy log-only behaviour —
	// the lap then merely ages out of Window). In production this is
	// *coord.ProdSettler, the same at-most-once marker OnDone writes, so the
	// settle is idempotent across elects and audit-logged.
	Settler reattachSettler
	// FailTTL bounds how long a Submit-failing unsettled lap stays a candidate
	// before it is settled follow_error; <= 0 uses DefaultReattachFailTTL.
	FailTTL time.Duration
	// Log records the one-pass outcome and per-lap re-attach failures. Nil
	// discards.
	Log func(format string, args ...any)
}

// Start runs the one re-attach pass and returns. It is best-effort: a lap whose
// supervisor is already gone fails Submit (Begin cannot reach it), which is
// logged and skipped — that pod either settled via S2 already or is left for the
// TTL backstop (§5 option b), never blocking the remaining laps. A failing lap
// older than FailTTL is additionally terminally settled follow_error (ISI-7540)
// so it is never re-Submitted on later elects — the reaper owns it from there.
// A reader error aborts the pass but never the operator (returns nil): the next
// elected leader re-runs it.
func (r *ReattachFollows) Start(ctx context.Context) error {
	if r.Dispatcher == nil || r.Reader == nil {
		r.logf("a2a follow re-attach skipped: dispatcher or reader unset (ledger-only operator)")
		return nil
	}
	window := r.Window
	if window <= 0 {
		window = DefaultReattachWindow
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
			r.logf("a2a follow re-attach interrupted after %d/%d laps: %v", reattached+failed, len(targets), err)
			return nil
		}
		if err := r.Dispatcher.Submit(ctx, t.A2ATaskID, t.RunID); err != nil {
			// Supervisor gone (Begin unreachable) or a transient submit error:
			// best-effort, leave this pod to S2 / the TTL backstop and move on.
			failed++
			r.logf("a2a follow re-attach failed for lap %s (run %s); leaving to reaper: %v", t.A2ATaskID, t.RunID, err)
			// ISI-7540: a lap that has been failing for longer than FailTTL past
			// its dispatch is a corpse (dead sandbox — it can never settle).
			// Terminally settle it follow_error so UnsettledDispatches stops
			// returning it on every leader-elect (no more re-Submit / gate
			// re-grant for a corpse); the reaper owns the lap from here. The
			// outcome keeps A2AFollowGateOpen closed, so the run still finalizes
			// through the death / retry path with its real budget. A zero
			// DispatchedAt (a reader that cannot age the lap) never settles here —
			// time.Since(zero) would otherwise dwarf any TTL and settle on the
			// first failure; such a lap keeps the legacy log-only behaviour and
			// ages out of Window instead.
			if ttl := r.failTTL(); r.Settler != nil && !t.DispatchedAt.IsZero() && time.Since(t.DispatchedAt) > ttl {
				reason := fmt.Sprintf("re-attach Submit failing for >%s since dispatch (supervisor unreachable): %v", ttl, err)
				if serr := r.Settler.Settle(ctx, t.A2ATaskID, t.RunID, coord.SettleOutcomeFollowError, reason); serr != nil {
					// Settle is best-effort too: the next leader-elect retries it.
					r.logf("a2a follow re-attach: settling stale failing lap %s (run %s) failed: %v", t.A2ATaskID, t.RunID, serr)
				} else {
					settled++
					r.logf("a2a follow re-attach: lap %s (run %s) failing and unsettled for >%s; settled follow_error, reaper owns the lap", t.A2ATaskID, t.RunID, ttl)
				}
			}
			continue
		}
		reattached++
	}
	r.logf("a2a follow re-attach done: reattached=%d failed=%d settled=%d candidates=%d window=%s failTTL=%s",
		reattached, failed, settled, len(targets), window, r.failTTL())
	return nil
}

// failTTL resolves the corpse-settle TTL (FailTTL <= 0 → DefaultReattachFailTTL).
func (r *ReattachFollows) failTTL() time.Duration {
	if r.FailTTL > 0 {
		return r.FailTTL
	}
	return DefaultReattachFailTTL
}

func (r *ReattachFollows) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}
