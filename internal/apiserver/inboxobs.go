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

package apiserver

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/telemetry"
)

// ============================================================================
// Inbox aggregate-read observability — ISI-5621 / ISI-5539 WS-G G-1 (+ G-4
// truncation flag). Instruments GET /api/squad/inbox (internal/apiserver/inbox.go
// → Server.squadInbox): the two-data-plane join whose arms degrade INDEPENDENTLY
// and SILENTLY today (ADR-0026 §3.2 — a failed arm logs a line and contributes
// zero rows, so a zeroed arm is invisible on a dashboard). This file makes that
// degrade a first-class signal, and (G-4) flags when an arm hits its LIMIT cap so
// a fleet admin's truncated view is never silently incomplete (ADR-0026 §9).
//
// The laws this file obeys (mirroring obsfunnel.go / cardinality_allowlist.go):
//   - metric label keys are a CLOSED enum: "arm" (5 values) × "fleet" (bool).
//     NEVER a ticket/run/user id or team UID on a metric (that is the "cardinality
//     is the enemy" Critical Rule). Team rides as a SPAN attribute only.
//   - degrade events carry an error CLASS, never the error message (PII posture —
//     the human-readable error is already in the handler's log line).
//   - instruments are created lazily on first handler use so they bind to the
//     MeterProvider telemetry.Setup installed, not the pre-Setup no-op meter.
// ============================================================================

// Inbox arm identifiers — the single metric label "arm" (closed enum). review,
// proposal and decision are the three Postgres corpus arms (each LIMIT-capped);
// cache is the controller-runtime informer read (fatal on error, §3 join source);
// marker is the per-principal read-marker arm (degrades to all-unread).
const (
	inboxArmReview   = "review"
	inboxArmProposal = "proposal"
	inboxArmDecision = "decision"
	inboxArmCache    = "cache"
	inboxArmMarker   = "marker"
)

// inboxArmCap is the per-arm LIMIT applied by every Postgres corpus arm
// (pkg/coord.reviewItemLimit, discussion/proposal.go LIMIT 500,
// discussion.inboxArmLimit — all 500, ADR-0026 §9). The store arms do not return
// a "hit cap" boolean, so G-4 degrades to the rows==cap heuristic the plan's Risk
// note (§7) anticipated: rows >= cap ⇒ the read was (probably) truncated. Keep in
// lockstep with those three constants if the fleet fan-out bound is ever retuned.
const inboxArmCap = 500

// inboxObs holds the aggregate-read instruments (WS-G G-1/G-4). Created once,
// lazily, on the first instrumented request — after telemetry.Setup ran.
type inboxObs struct {
	readDuration metric.Float64Histogram // ksquad.inbox.read.duration{fleet}        → ..._duration_seconds
	readRows     metric.Int64Histogram   // ksquad.inbox.read.rows{arm,fleet}
	armFailures  metric.Int64Counter     // ksquad.inbox.arm.failures{arm}           → ..._failures_total
	armTruncated metric.Int64Counter     // ksquad.inbox.arm.truncated{arm}          → ..._truncated_total
}

var (
	inboxObsOnce    sync.Once
	inboxObsOnceVal *inboxObs
)

// inboxInst returns the process-wide inbox-read instruments, creating them on
// first use (see the file-header law about lazy creation).
func inboxInst() *inboxObs {
	inboxObsOnce.Do(func() {
		m := telemetry.Meter()
		o := &inboxObs{}
		o.readDuration, _ = m.Float64Histogram("ksquad.inbox.read.duration",
			metric.WithUnit("s"),
			metric.WithDescription("Inbox two-plane aggregate-read latency (ISI-5621, WS-G G-1)."))
		o.readRows, _ = m.Int64Histogram("ksquad.inbox.read.rows",
			metric.WithUnit("{row}"),
			metric.WithDescription("Rows contributed per Inbox arm — a drop-to-zero on one arm while others are healthy is a visible anomaly, not a silent degrade (ISI-5621, WS-G G-1)."))
		o.armFailures, _ = m.Int64Counter("ksquad.inbox.arm.failures",
			metric.WithUnit("{failure}"),
			metric.WithDescription("Inbox arm degradations — increments where today only a log.Printf fires (ADR-0026 §3.2, ISI-5621 WS-G G-1)."))
		o.armTruncated, _ = m.Int64Counter("ksquad.inbox.arm.truncated",
			metric.WithUnit("{truncation}"),
			metric.WithDescription("Inbox arm reads that hit their LIMIT cap — a fleet admin's truncated view, made visible (ADR-0026 §9, ISI-5621 WS-G G-4 no-silent-truncation)."))
		inboxObsOnceVal = o
	})
	return inboxObsOnceVal
}

// inboxReadStart opens the domain span `inbox.read` off the request context. The
// span name is the low-cardinality domain operation, not the HTTP route (the
// otelhttp wrapper already gives the HTTP server span). Returns the child context
// so every arm call becomes a child span, plus the start instant for the latency
// histogram recorded in finish.
func inboxReadStart(ctx context.Context) (context.Context, trace.Span, time.Time) {
	ctx, span := telemetry.Tracer().Start(ctx, "inbox.read")
	return ctx, span, time.Now()
}

// recordArm attributes one arm's outcome onto the span and the metrics. An arm
// that is not ok increments arm.failures and records an inbox.arm_degraded span
// event with the error CLASS (never the message). An ok arm records its row count
// and, when capped and rows>=cap, flags truncation (span attr + event + counter).
func (o *inboxObs) recordArm(ctx context.Context, span trace.Span, arm string, fleet bool, rows int, ok, capped bool, err error) {
	span.SetAttributes(
		attribute.Int("ksquad.inbox.arm."+arm+".rows", rows),
		attribute.Bool("ksquad.inbox.arm."+arm+".ok", ok),
	)
	if !ok {
		o.armFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("arm", arm)))
		span.AddEvent("inbox.arm_degraded", trace.WithAttributes(
			attribute.String("arm", arm),
			attribute.String("error.type", inboxErrClass(err)),
		))
		return
	}
	o.readRows.Record(ctx, int64(rows), metric.WithAttributes(
		attribute.String("arm", arm),
		attribute.Bool("fleet", fleet),
	))
	if capped && rows >= inboxArmCap {
		span.SetAttributes(attribute.Bool("ksquad.inbox.arm."+arm+".truncated", true))
		span.AddEvent("inbox.arm_truncated", trace.WithAttributes(
			attribute.String("arm", arm),
			attribute.Int("cap", inboxArmCap),
		))
		o.armTruncated.Add(ctx, 1, metric.WithAttributes(attribute.String("arm", arm)))
	}
}

// fatalArm records a hard arm failure (today only the cache arm — an informer
// read error fails the whole read with 502, it does not degrade to zero rows) and
// marks the span status Error so the trace reflects the non-degrade path.
func (o *inboxObs) fatalArm(ctx context.Context, span trace.Span, arm, msg string, err error) {
	o.recordArm(ctx, span, arm, false, 0, false, false, err)
	span.SetStatus(codes.Error, msg)
}

// finish stamps the read-wide bounded attributes (fleet on the span, team.id on
// the span ONLY — never a metric label) and records the latency histogram. Call
// exactly once, immediately before the handler writes its 200.
func (o *inboxObs) finish(ctx context.Context, span trace.Span, start time.Time, fleet bool, teamID string, itemCount int) {
	span.SetAttributes(
		attribute.Bool("ksquad.inbox.fleet", fleet),
		attribute.String("ksquad.team.id", teamID),
		attribute.Int("ksquad.inbox.item_count", itemCount),
	)
	o.readDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
		attribute.Bool("fleet", fleet),
	))
}

// inboxErrClass maps an arm error to a bounded class for the degrade span event,
// never exposing the message (PII posture — the message is in the handler log).
func inboxErrClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "error"
	}
}
