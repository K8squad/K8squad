package events

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/telemetry"
)

// trace.go joins the outbox → NATS relay hop to the run trace (ISI-4440,
// follow-up to ISI-4238). The run's distributed trace reaches the sandbox as a
// W3C carrier, but the eventing hop was invisible: the relay published a bare
// body with no trace headers and never opened a span. Here the relay restores
// the run's span context from the row (the trace_carrier column stamped at
// capture, or the payload's trace_id as a fallback), opens a producer span, and
// injects the carrier so the consumer can continue the same trace — following
// OTel messaging semantic conventions (messaging.system=nats, operation
// publish/receive, destination = the NATS subject).

const (
	// messagingSystemNATS et al. are the OTel messaging semantic-convention
	// attribute keys used verbatim (the upstream constants live behind a
	// module we deliberately don't pull in for four string keys).
	attrMessagingSystem      = "messaging.system"
	attrMessagingOperation   = "messaging.operation"
	attrMessagingDestination = "messaging.destination.name"
	attrKsquadRunRef         = "ksquad.run.id"
	messagingSystemNATS      = "nats"
)

// runTraceContext restores the run's span context for a row so a span started
// from the returned context shares the run's trace_id (joining the run trace).
// It prefers the trace_carrier column (a full W3C traceparent stamped live at
// capture — yields true parent/child nesting); failing that it reconstructs a
// remote span context from the payload's trace_id (WS-D lifecycle rows carry it,
// ISI-4386) so the event still lands in the trace, if not perfectly nested. When
// neither is present it returns ctx unchanged and the hop roots a fresh trace.
func runTraceContext(ctx context.Context, row OutboxRow) context.Context {
	if len(row.TraceCarrier) > 0 {
		return telemetry.Extract(ctx, row.TraceCarrier)
	}
	if tid := traceIDFromPayload(row.Payload); tid != (trace.TraceID{}) {
		// A trace_id alone has no span id; synthesize a valid non-zero remote
		// parent so the SpanContext is usable. The child shares the run trace_id
		// (appears in the trace); the synthetic parent is a harmless placeholder.
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    tid,
			SpanID:     syntheticSpanID(tid),
			TraceFlags: trace.FlagsSampled,
			Remote:     true,
		})
		return trace.ContextWithRemoteSpanContext(ctx, sc)
	}
	return ctx
}

// traceIDFromPayload pulls a 32-hex `trace_id` out of a lifecycle payload,
// returning the zero TraceID when absent or malformed (best effort).
func traceIDFromPayload(payload []byte) trace.TraceID {
	if len(payload) == 0 {
		return trace.TraceID{}
	}
	var body struct {
		TraceID string `json:"trace_id"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.TraceID == "" {
		return trace.TraceID{}
	}
	tid, err := trace.TraceIDFromHex(body.TraceID)
	if err != nil {
		return trace.TraceID{}
	}
	return tid
}

// syntheticSpanID derives a stable, valid (non-zero) span id from the trace id so
// the reconstructed remote parent is well-formed. It is a placeholder, never a
// real emitted span — only the child producer span is exported.
func syntheticSpanID(tid trace.TraceID) trace.SpanID {
	var sid trace.SpanID
	copy(sid[:], tid[:8])
	if sid == (trace.SpanID{}) {
		sid[0] = 1 // never all-zero (that is the invalid span id)
	}
	return sid
}

// injectCarrier serializes ctx's active span context into a fresh W3C carrier
// map for the outbound message headers. Empty when ctx has no span.
func injectCarrier(ctx context.Context) map[string]string {
	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)
	return carrier
}

// messagingAttrs builds the OTel messaging semconv attribute set shared by the
// producer and consumer spans.
func messagingAttrs(operation, subject, runID string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(attrMessagingSystem, messagingSystemNATS),
		attribute.String(attrMessagingOperation, operation),
		attribute.String(attrMessagingDestination, subject),
	}
	if runID != "" {
		attrs = append(attrs, attribute.String(attrKsquadRunRef, runID))
	}
	return attrs
}

// extractCarrier lifts a W3C carrier out of consumer-side message headers into
// ctx so a consumer span continues the producer's trace. telemetry.Extract only
// reads traceparent/tracestate, so passing the full header map is safe.
func extractCarrier(ctx context.Context, headers map[string]string) context.Context {
	return telemetry.Extract(ctx, headers)
}
