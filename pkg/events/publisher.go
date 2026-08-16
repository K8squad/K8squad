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

// LagReporter is an OPTIONAL capability a Publisher may also implement to
// surface the §17.2 JetStream consumer-lag signal (total messages pending
// across the stream's durable consumers). The relay type-asserts for it; a
// Publisher that does not implement it simply reports consumer_lag as 0.
type LagReporter interface {
	ConsumerLag(ctx context.Context) (int64, error)
}
