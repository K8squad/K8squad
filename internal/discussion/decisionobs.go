// decision_request lifecycle observability — ISI-5619 / ISI-5539 WS-G G-2.
//
// Instruments the four store transitions in decision.go (create / answer / reject /
// expire(+supersede)) with semconv-style span events and the Inbox's core metrics.
// The store is the single source of truth: the apiserver handlers are thin, so the
// signal lives here where every transition — console, agent, or the anti-nag sweep —
// passes through exactly once.
//
// The laws this file obeys (mirroring inboxobs.go / cardinality_allowlist.go):
//   - metric label keys are a CLOSED enum: "mode" (4 values) × "phase" (5 values) on
//     the funnel counter, "mode" × "terminal_phase" on the resolution histogram. A
//     work-item / principal / team id is NEVER a metric label (the "cardinality is
//     the enemy" Critical Rule); work_item.ref + team.id ride span EVENT attributes.
//   - events carry SHAPE only — mode, counts, booleans — never the question text,
//     option labels, free-text answer, or reject reason (PII posture, ADR-0026 §10;
//     the human-readable content already lives in the thread messages).
//   - instruments are created lazily on first transition so they bind to the
//     MeterProvider telemetry.Setup installed, not the pre-Setup no-op meter.
package discussion

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/telemetry"
)

// decisionObs holds the decision_request lifecycle instruments (WS-G G-2). Created
// once, lazily, on the first transition — after telemetry.Setup ran.
type decisionObs struct {
	requests   metric.Int64Counter     // ksquad.decision_requests{mode,phase}            → ..._total
	resolution metric.Float64Histogram // ksquad.decision_request.resolution{mode,terminal_phase} → ..._seconds
}

// decisionPhaseCreated is the funnel-entry phase label on ksquad.decision_requests.
// It is deliberately distinct from the DB lifecycle value DecisionPhaseOpen: the
// counter measures the transition "a card was created", not the resting state.
const decisionPhaseCreated = "created"

var (
	decisionObsOnce    sync.Once
	decisionObsOnceVal *decisionObs
)

// decisionInst returns the process-wide decision_request instruments, creating them
// on first use (see the file-header law about lazy creation).
func decisionInst() *decisionObs {
	decisionObsOnce.Do(func() {
		m := telemetry.Meter()
		o := &decisionObs{}
		o.requests, _ = m.Int64Counter("ksquad.decision_requests",
			metric.WithUnit("{transition}"),
			metric.WithDescription("decision_request lifecycle transitions by mode and phase (created/answered/rejected/expired/superseded) — the Inbox decision funnel (ISI-5619, WS-G G-2)."))
		o.resolution, _ = m.Float64Histogram("ksquad.decision_request.resolution",
			metric.WithUnit("s"),
			metric.WithDescription("Time a decision_request sat open before it resolved (created→answered/rejected/expired) — the Inbox core SLI: how long humans sit on a decision (ISI-5619, WS-G G-2)."))
		decisionObsOnceVal = o
	})
	return decisionObsOnceVal
}

// decisionAuthorKind derives the bounded agent|human enum from the caller's identity
// (AgentID set ⇒ agent-authored, ADR-0026), never the free-text principal.
func decisionAuthorKind(auth AuthorContext) string {
	if auth.AgentID != nil {
		return "agent"
	}
	return "human"
}

// decisionModeFromPayload reads just the bounded `mode` off a decision_request
// message payload for the metric labels, ignoring the (PII-bearing) rest.
func decisionModeFromPayload(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Mode
}

// recordDecisionCreated emits the decision_request.created span event (shape only)
// and, for a real create (not an idempotent-key replay that returned the existing
// card), counts it on the funnel. The idempotent replay still records the event —
// carrying idempotent_hit=true — so a retry storm is visible, but adds no funnel row
// because no new card was born.
func recordDecisionCreated(ctx context.Context, mode string, idempotentHit bool, auth AuthorContext, workItemRef, teamID string) {
	trace.SpanFromContext(ctx).AddEvent("decision_request.created", trace.WithAttributes(
		attribute.String("ksquad.decision.mode", mode),
		attribute.Bool("ksquad.decision.idempotent_hit", idempotentHit),
		attribute.String("ksquad.decision.author_kind", decisionAuthorKind(auth)),
		attribute.String("ksquad.work_item.ref", workItemRef),
		attribute.String("ksquad.team.id", teamID),
	))
	if !idempotentHit {
		decisionInst().requests.Add(ctx, 1, metric.WithAttributes(
			attribute.String("mode", mode),
			attribute.String("phase", decisionPhaseCreated),
		))
	}
}

// recordDecisionResolved counts a terminal transition on the funnel and records the
// created→now resolution latency (the "how long did a human sit on it" SLI), both
// labeled by bounded enums only. A zero createdAt (payload/row we could not time)
// skips the latency sample rather than recording a bogus duration.
func recordDecisionResolved(ctx context.Context, mode, terminalPhase string, createdAt time.Time) {
	inst := decisionInst()
	inst.requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("mode", mode),
		attribute.String("phase", terminalPhase),
	))
	if !createdAt.IsZero() {
		inst.resolution.Record(ctx, time.Since(createdAt).Seconds(), metric.WithAttributes(
			attribute.String("mode", mode),
			attribute.String("terminal_phase", terminalPhase),
		))
	}
}

// emitDecisionAnswered / emitDecisionRejected / emitDecisionExpired add the matching
// lifecycle span event with SHAPE-only attributes (never the answer/reason text).
func emitDecisionAnswered(ctx context.Context, mode string, freeText bool, selectedCount int, workItemRef string) {
	trace.SpanFromContext(ctx).AddEvent("decision_request.answered", trace.WithAttributes(
		attribute.String("ksquad.decision.mode", mode),
		attribute.Bool("ksquad.decision.free_text", freeText),
		attribute.Int("ksquad.decision.selected_count", selectedCount),
		attribute.String("ksquad.work_item.ref", workItemRef),
	))
}

func emitDecisionRejected(ctx context.Context, mode string, reasonGiven bool, workItemRef string) {
	trace.SpanFromContext(ctx).AddEvent("decision_request.rejected", trace.WithAttributes(
		attribute.String("ksquad.decision.mode", mode),
		attribute.Bool("ksquad.decision.reject_reason_given", reasonGiven),
		attribute.String("ksquad.work_item.ref", workItemRef),
	))
}

// emitDecisionExpired covers both the expired and superseded terminal phases. Only
// the revision-moved sweep (SupersedeStaleDecisionRequests) exists as a supersede
// cause today; a later_comment cause is reserved by ADR-0026 §4.2 but not yet wired,
// so the attribute is set only for superseded and omitted for a plain expiry.
func emitDecisionExpired(ctx context.Context, phase, supersedeCause, workItemRef string) {
	attrs := []attribute.KeyValue{
		attribute.String("ksquad.decision.phase", phase),
		attribute.String("ksquad.work_item.ref", workItemRef),
	}
	if supersedeCause != "" {
		attrs = append(attrs, attribute.String("ksquad.decision.supersede_cause", supersedeCause))
	}
	trace.SpanFromContext(ctx).AddEvent("decision_request.expired", trace.WithAttributes(attrs...))
}
