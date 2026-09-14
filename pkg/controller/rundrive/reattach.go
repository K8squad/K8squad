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

	targets, err := r.Reader.UnsettledDispatches(ctx, window)
	if err != nil {
		r.logf("a2a follow re-attach: list unsettled dispatches failed (skipping this pass): %v", err)
		return nil
	}

	var reattached, failed int
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
			continue
		}
		reattached++
	}
	r.logf("a2a follow re-attach done: reattached=%d failed=%d candidates=%d window=%s",
		reattached, failed, len(targets), window)
	return nil
}

func (r *ReattachFollows) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}
