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

package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// The CONSUMER half of story 5.10 (this file). The producer + wire contract
// (pkg/a2a: EventRateLimited, RateLimitedPayload with a RAW Retry-After;
// pkg/shim emits it) landed in ISI-2894. Here the core-side A2A client turns
// that standardized signal into a control-plane action:
//
//	EventRateLimited → RawRateLimit → NormalizeRateLimited → OnRateLimited
//	   → SwitchModel (5.11, handed to the switch seam) | PauseRun (2.11 coord.Pause)
//
// The single canonical Retry-After parse lives in modelendpoint (the shim ships
// the header RAW, per RateLimitedPayload), so an HTTP-date window is resolved
// against the CONSUMER's clock — here — never the producer's.
//
// The bridge stays in internal/a2a (the consumer). It imports pkg/modelendpoint
// (the transport-agnostic decision core) and pkg/a2a (transport+types), which
// are deliberately kept from importing each other. It does NOT import pkg/coord:
// the durable-pause and switch executions ride injected seams (Pauser /
// SwitchExecutor), exactly as Dispatcher keeps coord/DB out via TaskBuilder —
// so the dependency stays one-way and every path is unit-testable against fakes.

// RunContext locates the Run/coord state that a rate_limited event maps to. The
// reconciler supplies the resolver; internal/a2a stays free of coord/DB deps.
type RunContext struct {
	// Agent is the Run's Agent, consulted by OnRateLimited to resolve a
	// configured fallback endpoint and to default FromModel to spec.model.
	Agent *api.Agent
	// WorkItem is the coord work_item_id to Pause on the PauseRun path (2.11).
	WorkItem int
	// RunID identifies the Run for the SwitchModel seam and for diagnostics.
	RunID string
}

// RunResolver maps a rate_limited event to its RunContext. ok=false drops the
// signal without erroring — the Run already settled, or the event's task is
// unknown to this process (an at-least-once redelivery after resume). A real
// lookup failure returns a non-nil error, which aborts the follow.
type RunResolver func(ctx context.Context, ev wire.Event) (rc RunContext, ok bool, err error)

// Pauser durably pauses a work item on the 2.11 scheduled-resume path
// (coord.ResumeStore.Pause). retryAfter is the provider window when the signal
// carried one, or nil for the exponential-backoff path. Injected so this
// package does not import pkg/coord.
type Pauser func(ctx context.Context, workItem int, retryAfter *time.Duration) error

// SwitchExecutor acts on a SwitchModel verdict (5.11): the SAME Run continues on
// the resolved fallback endpoint, keeping its claim. It is optional — a nil
// executor makes a SwitchModel verdict a no-op here, deferring the switch to the
// shim-driven path (5.8) while the Pause path stays fully wired.
type SwitchExecutor func(ctx context.Context, runID string, plan modelendpoint.SwitchPlan) error

// RateLimitBridge is the core-side consumer of the standardized rate_limited
// signal (story 5.10). It is stateless beyond its seams and safe for concurrent
// use if they are.
type RateLimitBridge struct {
	// Switcher is the 5.11 decision core (SwitchModel vs PauseRun). Required.
	Switcher *modelendpoint.Switcher
	// Resolve maps an event to its Run context. Required.
	Resolve RunResolver
	// Pause drives the durable Run→Paused on the PauseRun verdict. Required.
	Pause Pauser
	// OnSwitch executes a SwitchModel verdict. Optional (nil → no-op).
	OnSwitch SwitchExecutor
}

// Handle processes ONE event. For every non-rate-limited type it is a no-op; on
// EventRateLimited it runs normalize → decide → act. A malformed payload or an
// unresolved Run is dropped (nil), never fatal — the signal is advisory and the
// task keeps running on its own terms. Genuine failures (resolver error, the
// 5.11 fail-closed unresolvable-fallback error, a Pause/switch error) propagate
// so the caller can surface them.
func (b *RateLimitBridge) Handle(ctx context.Context, ev wire.Event) error {
	if ev.Type != wire.EventRateLimited {
		return nil
	}
	pay, ok := rateLimitedPayload(ev.Payload)
	if !ok {
		// A rate_limited event with an undecodable payload carries no window
		// and no model: nothing actionable, so forward-and-ignore.
		return nil
	}

	rc, ok, err := b.Resolve(ctx, ev)
	if err != nil {
		return fmt.Errorf("a2a: resolve run for rate-limited task %s: %w", ev.A2ATaskID, err)
	}
	if !ok {
		return nil
	}

	// Build the transport-agnostic view and normalize through the ONE gate.
	// ObservedAt = ev.TS anchors an HTTP-date Retry-After to the event's
	// instant (consumer clock), not wall-now.
	raw := modelendpoint.RawRateLimit{
		StatusCode: modelendpoint.StatusTooManyRequests,
		RetryAfter: pay.RetryAfter,
		FromModel:  pay.Model,
		ObservedAt: ev.TS,
	}
	sig, ok := modelendpoint.NormalizeRateLimited(raw)
	if !ok {
		// Defensive: a rate_limited event always normalizes (429), but if the
		// gate ever rejects it there is nothing to act on.
		return nil
	}

	plan, err := b.Switcher.OnRateLimited(ctx, rc.Agent, sig)
	if err != nil {
		// 5.11 fails CLOSED on a configured-but-unresolvable fallback: surface
		// it rather than silently hammering the throttled primary.
		return fmt.Errorf("a2a: rate-limit decision for run %s: %w", rc.RunID, err)
	}

	switch plan.Action {
	case modelendpoint.ActionPauseRun:
		// 2.11 honours the provider window when present; a zero window means
		// "no Retry-After" and hands coord.Pause its exponential backoff.
		var retryAfter *time.Duration
		if sig.RetryAfter > 0 {
			d := sig.RetryAfter
			retryAfter = &d
		}
		if err := b.Pause(ctx, rc.WorkItem, retryAfter); err != nil {
			return fmt.Errorf("a2a: pause run %s (work item %d): %w", rc.RunID, rc.WorkItem, err)
		}
		return nil
	case modelendpoint.ActionSwitchModel:
		if b.OnSwitch == nil {
			return nil
		}
		if err := b.OnSwitch(ctx, rc.RunID, plan); err != nil {
			return fmt.Errorf("a2a: switch model for run %s: %w", rc.RunID, err)
		}
		return nil
	default:
		return nil
	}
}

// Sink wraps inner so that every EventRateLimited event drives the 5.10
// consumer action (Handle) BEFORE it is forwarded to inner (the run_events
// live surface). This is the zero-touch integration seam: the reconciler wraps
// its run-events sink once when constructing the Dispatcher, and Follow's
// existing fan-out carries the signal to the bridge with no client.go change.
// A nil inner discards forwarded events (the ledger-only posture); the bridge
// still runs. A Handle error aborts the dispatch (fail-closed), consistent with
// EventSink's contract that a sink error cancels the live task.
func (b *RateLimitBridge) Sink(inner EventSink) EventSink {
	if inner == nil {
		inner = DiscardSink
	}
	return SinkFunc(func(ctx context.Context, ev wire.Event) error {
		if err := b.Handle(ctx, ev); err != nil {
			return err
		}
		return inner.Event(ctx, ev)
	})
}

// rateLimitedPayload normalizes an EventRateLimited payload into the typed
// RateLimitedPayload, tolerating both the concrete in-process value and the
// JSON-decoded form the stdio transport carries (mirrors artifactRef /
// statusPayload). An empty payload (no model, no window) is a valid 429 with no
// provider window — the backoff path — so it decodes to the zero value with
// ok=true; only an undecodable payload returns ok=false.
func rateLimitedPayload(payload any) (wire.RateLimitedPayload, bool) {
	switch v := payload.(type) {
	case wire.RateLimitedPayload:
		return v, true
	case *wire.RateLimitedPayload:
		if v == nil {
			return wire.RateLimitedPayload{}, false
		}
		return *v, true
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return wire.RateLimitedPayload{}, false
		}
		var rp wire.RateLimitedPayload
		if err := json.Unmarshal(b, &rp); err != nil {
			return wire.RateLimitedPayload{}, false
		}
		return rp, true
	}
}
