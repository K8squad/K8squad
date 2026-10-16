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

// ratelimitbridge.go — the PRODUCTION wiring of the 5.10/5.11 rate-limit
// consumer (ISI-6054). internal/a2a.RateLimitBridge is the transport-agnostic
// decision core (EventRateLimited → Switcher.OnRateLimited → SwitchModel |
// PauseRun); this file binds its three seams to the operator's real world:
//
//   - Resolve: the sink is per-Run (SinkFor), so the resolver binds the Run's
//     UID and maps any event to that Run's Agent (the fallback config source).
//   - Pause: the 2.11/3.7 park — guarded step move into paused(rate_limited)
//     (ProdClaims.PauseRateLimited) plus the single durable wake episode with
//     the provider's Retry-After (ProdResumeStore.Pause, the same store/table
//     the driver's timer and park path already drive).
//   - OnSwitch: the SwitchModel verdict's durable half — the Run's open
//     modelSegments portion is CLOSED with reason rate_limited. That closed
//     segment is the switch marker buildTask consults on the next lap (see
//     fallbackActive): the SAME Run re-dispatches on the tier's fallback
//     endpoint, keeping its checkout and coordination identity. The in-turn
//     handoff itself is the retry lap's job — the throttled attempt fails on
//     its own terms, the drive loop's existing retryOrFail provisions the new
//     lap, and the marker routes that lap to the fallback.
//
// BOTH transport legs (legacy sandbox supervisor POST and the AgentMesh
// gateway /task route) land here unchanged: they share the per-Run sink this
// file wraps, and they share buildTask for the re-dispatch.
package rundrive

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	a2a "github.com/K8squad/K8squad/internal/a2a"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// fallbackActive reports whether this Run carries the 5.11 switch marker: a
// provenance portion closed by reason rate_limited. Once stamped, the marker is
// sticky for the remainder of the Run — every later lap dispatches on the
// winning tier's fallback pair, keeping the checkout (no re-intake, no
// ping-pong: a fallback that itself 429s re-closes its portion and re-opens on
// the same fallback at the next lap) — while a FRESH Run starts clean on the
// primary.
func fallbackActive(run *api.Run) bool {
	for _, s := range run.Status.ModelSegments {
		if s.Reason == modelendpoint.ReasonRateLimited {
			return true
		}
	}
	return false
}

// rateLimitBridge builds the 5.10 consumer bound to ONE Run's event stream.
// The returned bridge is stateless beyond its seams; wrapping the per-Run sink
// (SinkFor) is the zero-touch integration the reconciler-side seam designed
// for.
func (d *operatorDispatch) rateLimitBridge(runID string) *a2a.RateLimitBridge {
	return &a2a.RateLimitBridge{
		Switcher: &modelendpoint.Switcher{Resolver: &modelendpoint.Resolver{Reader: d.cfg.Client}},
		Resolve:  d.runResolver(runID),
		Pause:    d.parker(runID),
		OnSwitch: d.switchExecutor(runID),
	}
}

// runResolver maps the per-Run sink's events onto the RunContext the decision
// core needs: the Run's dispatch Agent (the fallback config source) and the
// run id for diagnostics. A Run (or Agent) that no longer resolves is dropped
// quietly (ok=false) — the Run settled or was deleted mid-stream; a settled
// Run has no fallback decision to make. Any other read failure is a real
// error and aborts the follow, per the fail-closed sink contract.
func (d *operatorDispatch) runResolver(runID string) a2a.RunResolver {
	return func(ctx context.Context, _ wire.Event) (a2a.RunContext, bool, error) {
		rc := a2a.RunContext{RunID: runID}
		run, err := d.runByUID(ctx, runID)
		if apierrors.IsNotFound(err) {
			return rc, false, nil
		}
		if err != nil {
			return rc, false, fmt.Errorf("rundrive: resolve Run %s for rate-limited event: %w", runID, err)
		}
		if len(run.Spec.Agents) == 0 {
			return rc, false, nil // no dispatch agent: no fallback tier to consult
		}
		ref := run.Spec.Agents[0]
		ns := ref.Namespace
		if ns == "" {
			ns = run.Namespace
		}
		var agent api.Agent
		if err := d.cfg.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &agent); err != nil {
			if apierrors.IsNotFound(err) {
				return rc, false, nil
			}
			return rc, false, fmt.Errorf("rundrive: resolve Agent %s/%s for rate-limited event: %w", ns, ref.Name, err)
		}
		rc.Agent = &agent
		return rc, true, nil
	}
}

// parker is the PauseRun verdict's executor (no fallback configured): the
// guarded step move into paused(rate_limited) plus the single durable wake
// episode carrying the provider's Retry-After window. The wake is the wired
// 3.7 timer's (ProdTimer over the same run_pause table) — ResumeDue →
// driver.OnResumeDue → RequeuePaused → dispatching. The workItem int the seam
// carries is ignored: coord work items are uuid strings, and this Run's ref is
// read straight off the Run CR (the sink is per-Run).
func (d *operatorDispatch) parker(runID string) a2a.Pauser {
	return func(ctx context.Context, _ int, retryAfter *time.Duration) error {
		if d.pauses == nil || d.claims == nil {
			return nil // ledger-only lane: no durable pause surface wired
		}
		run, err := d.runByUID(ctx, runID)
		if apierrors.IsNotFound(err) {
			return nil // the Run settled; nothing to park
		}
		if err != nil {
			return fmt.Errorf("rundrive: resolve Run %s to park: %w", runID, err)
		}
		if _, err := d.claims.PauseRateLimited(ctx, run.Spec.WorkItemRef); err != nil {
			return fmt.Errorf("rundrive: park run %s on rate limit: %w", runID, err)
		}
		if _, err := d.pauses.Pause(ctx, run.Spec.WorkItemRef, runID, retryAfter); err != nil {
			return fmt.Errorf("rundrive: record rate-limit wake for run %s: %w", runID, err)
		}
		return nil
	}
}

// switchExecutor is the SwitchModel verdict's executor: it closes the Run's
// open provenance portion with reason rate_limited — the marker buildTask
// routes the next lap on (fallbackActive). Idempotent by construction: with no
// open portion there is nothing to close (a repeat signal while already
// switched is a no-op), and a portion already serving the fallback model is
// left open (the run IS on the fallback — re-closing it would churn the
// provenance log without changing the route). Best-effort on the status
// write, exactly like recordModelProvenance: a failed stamp is logged and the
// next rate_limited signal retries it; provenance is observability and must
// not cancel a live task.
func (d *operatorDispatch) switchExecutor(runID string) a2a.SwitchExecutor {
	return func(ctx context.Context, _ string, plan modelendpoint.SwitchPlan) error {
		run, err := d.runByUID(ctx, runID)
		if apierrors.IsNotFound(err) {
			return nil // the Run settled; nothing to switch
		}
		if err != nil {
			return fmt.Errorf("rundrive: resolve Run %s for model switch: %w", runID, err)
		}
		// Idempotency: already serving the fallback — nothing to close.
		for _, s := range run.Status.ModelSegments {
			if s.EndedAt == nil && s.Model == plan.To.Model {
				return nil
			}
		}
		// Close the latest OPEN portion (its own model names it — the signal's
		// FromModel may be empty and the serving tier may not be the agent's).
		var open string
		for _, s := range run.Status.ModelSegments {
			if s.EndedAt == nil {
				open = s.Model
			}
		}
		if open == "" {
			return nil
		}
		patched := run.DeepCopy()
		patched.Status.ModelSegments = modelendpoint.CloseSegment(
			run.Status.ModelSegments, open, metav1.Now(), modelendpoint.ReasonRateLimited)
		if err := d.cfg.Client.Status().Patch(ctx, patched, client.MergeFrom(run)); err != nil {
			slog.WarnContext(ctx, "rundrive: could not stamp model-fallback switch marker (best-effort)",
				"run.uid", runID, "run.name", run.Name, "run.namespace", run.Namespace,
				"from_model", open, "fallback_model", plan.To.Model, "err", err)
		}
		return nil
	}
}
