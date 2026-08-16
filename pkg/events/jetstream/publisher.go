// Package jetstream is the concrete NATS/JetStream Publisher for the event-seam
// relay (Story 12.1 / ISI-2260). It is isolated in its own package so that
// pkg/events — and everything that merely CAPTURES events (pkg/coord, the
// reconcilers) — compiles without the nats.go dependency; only the process that
// actually runs the relay links this.
//
// The stream this publishes to is provisioned by the Story 9.4 chart
// (templates/nats.yaml) with subject `ksquad.>`. The relay's isolation contract
// (event-relay.yaml: relay.blocksWritePath=false) is upheld here by construction:
// this publisher is handed to the relay worker, which runs out-of-band — nothing
// in this file is reachable from a write transaction.
package jetstream

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/K8squad/K8squad/pkg/events"
)

// StreamName is the JetStream stream the relay publishes into. Subjects
// ksquad.{entity}.{project}.{squad}.{event_type} all bind to it via ksquad.>.
const StreamName = "KSQUAD_EVENTS"

// Config binds the publisher to a bus. Only URL is required.
type Config struct {
	URL          string        // NATS URL (relay.natsUrl); e.g. nats://ksquad-nats:4222
	StreamName   string        // "" ⇒ StreamName
	SubjectGlob  string        // stream bind subject; "" ⇒ "<prefix>.>" ⇒ "ksquad.>"
	Prefix       string        // subject root; "" ⇒ events.DefaultPrefix
	PublishAckTO time.Duration // per-publish ack wait; 0 ⇒ 5s
	Options      []nats.Option // extra client options (TLS, creds, name)
}

// Publisher publishes acked JetStream messages and reports consumer lag. It
// implements events.Publisher and events.LagReporter.
type Publisher struct {
	nc              *nats.Conn
	js              jetstream.JetStream
	stream          string
	ackTO           time.Duration
	streamEnsureErr error // non-nil if the best-effort stream ensure at Connect failed
}

// StreamEnsureErr reports whether the best-effort stream provisioning at Connect
// failed (nil = the stream is ready, or the chart owns it). Callers may log it;
// it is deliberately NOT fatal, so the relay still starts and buffers.
func (p *Publisher) StreamEnsureErr() error { return p.streamEnsureErr }

// Connect dials NATS, binds the JetStream context, and ensures the events
// stream exists (idempotent create — CreateOrUpdateStream). It does NOT gate on
// the stream being reachable forever: if NATS later goes down, Publish simply
// errors and the relay retains the row (at-least-once).
func Connect(ctx context.Context, cfg Config) (*Publisher, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("jetstream.Connect: URL is required")
	}
	stream := cfg.StreamName
	if stream == "" {
		stream = StreamName
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = events.DefaultPrefix
	}
	glob := cfg.SubjectGlob
	if glob == "" {
		glob = prefix + ".>"
	}
	ackTO := cfg.PublishAckTO
	if ackTO <= 0 {
		ackTO = 5 * time.Second
	}

	// ISOLATION CONTRACT (§17.4, event-relay.yaml): NATS being down must NEVER
	// block the relay from starting — it buffers in the outbox and flushes when
	// the bus returns. So the client retries the initial connect and reconnects
	// forever rather than failing fast; callers prepend their own options, these
	// defaults win only when unset by convention (nats applies them in order).
	opts := append([]nats.Option{
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.Name("ksquad-event-relay"),
	}, cfg.Options...)
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("jetstream.Connect: dial %s: %w", cfg.URL, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream.Connect: jetstream context: %w", err)
	}
	p := &Publisher{nc: nc, js: js, stream: stream, ackTO: ackTO}
	// Ensure the stream exists — BEST-EFFORT. The Story 9.4 chart (nats.yaml)
	// owns stream provisioning in-cluster (de-dup guard); this only guarantees a
	// target for a standalone relay. A failure here (NATS still connecting, or
	// the chart will create it) must NOT fail startup — publishes will drive the
	// reconnect and buffer until the stream exists.
	ensureCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := js.CreateOrUpdateStream(ensureCtx, jetstream.StreamConfig{
		Name:      stream,
		Subjects:  []string{glob},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
	}); err != nil {
		p.streamEnsureErr = err
	}
	return p, nil
}

// Publish sends data to subject and BLOCKS until JetStream acks persistence, so
// a nil return means the event is durable on the bus and the relay may stamp
// published_at. Any error (NATS down, no ack) is returned so the relay retains
// the row for retry (at-least-once). msg-id de-dup is left to the caller's row
// id if configured on the stream; the relay's set-once stamp already bounds
// duplicates to a single redelivery on a mark-published failure.
func (p *Publisher) Publish(ctx context.Context, subject string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, p.ackTO)
	defer cancel()
	if _, err := p.js.Publish(ctx, subject, data); err != nil {
		return fmt.Errorf("jetstream.Publish(%s): %w", subject, err)
	}
	return nil
}

// ConsumerLag reports total messages pending across the stream's consumers (the
// §17.2 consumer_lag signal). Best-effort: summed NumPending over every consumer.
func (p *Publisher) ConsumerLag(ctx context.Context) (int64, error) {
	s, err := p.js.Stream(ctx, p.stream)
	if err != nil {
		return 0, fmt.Errorf("jetstream.ConsumerLag: stream %s: %w", p.stream, err)
	}
	var pending int64
	lister := s.ListConsumers(ctx)
	for ci := range lister.Info() {
		// NumPending is uint64; clamp before the int64 conversion so an
		// implausibly huge queue depth can't wrap negative (gosec G115).
		if ci.NumPending > math.MaxInt64 {
			return math.MaxInt64, nil
		}
		pending += int64(ci.NumPending)
	}
	if err := lister.Err(); err != nil {
		return pending, fmt.Errorf("jetstream.ConsumerLag: list consumers: %w", err)
	}
	return pending, nil
}

// Close drains and closes the NATS connection.
func (p *Publisher) Close() error {
	if p.nc != nil {
		p.nc.Close()
	}
	return nil
}

var (
	_ events.Publisher   = (*Publisher)(nil)
	_ events.LagReporter = (*Publisher)(nil)
)
