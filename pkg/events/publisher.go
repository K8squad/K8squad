package events

import "context"

// Publisher is the relay's view of the event bus: publish one composed subject +
// body, durably. The relay depends only on this interface, so the nats.go
// JetStream client stays in the pkg/events/jetstream subpackage and unit tests
// inject an in-memory fake. A Publish error (e.g. NATS down) leaves the row
// unflushed for the next tick — the interface's error IS the at-least-once
// retry signal.
type Publisher interface {
	// Publish sends data to subject. For JetStream it MUST block until the
	// server acks persistence, so a returned nil means the event is durable on
	// the bus and the relay may stamp published_at; any error leaves the row
	// unflushed (retried), never dropped.
	Publish(ctx context.Context, subject string, data []byte) error
	// Close releases the underlying connection.
	Close() error
}

// HeaderPublisher is an OPTIONAL capability a Publisher may also implement to
// carry per-message headers alongside the body. The relay uses it to inject the
// W3C trace carrier (traceparent/tracestate) into NATS message headers so the
// publish/consume hop joins the run trace (ISI-4440, OTel messaging semconv). A
// Publisher that does not implement it falls back to the header-less Publish —
// the event still flows, only without the trace headers, so this stays a purely
// additive capability (no falsification-bench fake has to change).
type HeaderPublisher interface {
	// PublishMsg sends data to subject with headers attached. Like Publish it
	// MUST block until the server acks persistence (at-least-once contract);
	// headers may be nil/empty (⇒ equivalent to Publish).
	PublishMsg(ctx context.Context, subject string, data []byte, headers map[string]string) error
}

// LagReporter is an OPTIONAL capability a Publisher may also implement to
// surface the §17.2 JetStream consumer-lag signal (total messages pending
// across the stream's durable consumers). The relay type-asserts for it; a
// Publisher that does not implement it simply reports consumer_lag as 0.
type LagReporter interface {
	ConsumerLag(ctx context.Context) (int64, error)
}
