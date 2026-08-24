// subscriber.go is the plugin-facing NATS subscribe SDK (Story 12.2 / ISI-2914,
// Arch §17.4). A plugin imports this to CONSUME the already-committed domain
// events the relay fans out onto KSQUAD_EVENTS — filtered by subject, decoded
// into the typed taxonomy (events.SubjectParts) plus the versioned payload — and
// nothing else.
//
// SCOPE GUARDRAIL (§17.4 no-P2P, Story 12.4): the SDK is EMIT-ONLY from the
// plugin's side, i.e. read-only. It deliberately exposes no Publish/write
// surface: a subscriber cannot inject a message back onto the bus or into
// coordination, so NATS stays a one-way projection of state that already
// committed to Postgres (ADR-001). The guardrail is enforced structurally — the
// type simply has no publish method — and asserted in subscriber_test.go.
package jetstream

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/K8squad/K8squad/pkg/events"
)

// Message is one delivered domain event as a plugin sees it: the taxonomy
// recovered from the subject (never from the body, §17.4) plus the raw versioned
// payload. The plugin unmarshals Payload itself against the event-catalog schema
// for EventType — the SDK stays payload-agnostic so a catalog version bump needs
// no SDK change.
type Message struct {
	events.SubjectParts
	Subject string // the raw delivered subject, for logging/tracing
	Payload []byte // versioned jsonb body; may be "{}" but never nil off the wire
}

// Handler processes one Message. Returning nil ACKs the message (it will not be
// redelivered); returning an error NAKs it for redelivery, so a plugin that
// fails to process an event does not silently drop it (at-least-once, C3).
type Handler func(ctx context.Context, msg Message) error

// SubscribeConfig binds a Subscriber to the bus. Only URL and Durable are
// required — Durable names the JetStream consumer so a plugin resumes from its
// last ack across restarts instead of replaying the whole stream.
type SubscribeConfig struct {
	URL        string        // NATS URL; e.g. nats://ksquad-nats:4222
	Durable    string        // durable consumer name; REQUIRED (per-plugin cursor)
	StreamName string        // "" ⇒ StreamName ("KSQUAD_EVENTS")
	Subjects   []string      // subject filters; empty ⇒ ["ksquad.>"] (all events)
	AckWait    time.Duration // redelivery timeout; 0 ⇒ 30s
	Options    []nats.Option // extra client options (TLS, creds, name)
}

// Subscriber is a durable, filtered consumer of KSQUAD_EVENTS. It owns its NATS
// connection; Close releases it. It has NO publish surface by construction
// (§17.4 guardrail).
type Subscriber struct {
	nc       *nats.Conn
	consumer jetstream.Consumer
}

// Subscribe dials NATS and binds (create-or-update) a durable pull consumer on
// the events stream, filtered to the configured subjects. It does NOT create the
// stream — the relay/chart owns provisioning; a subscriber only ever reads an
// existing stream, so a missing stream is a real error here (unlike the relay's
// best-effort ensure).
func Subscribe(ctx context.Context, cfg SubscribeConfig) (*Subscriber, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("jetstream.Subscribe: URL is required")
	}
	if cfg.Durable == "" {
		return nil, fmt.Errorf("jetstream.Subscribe: Durable is required (per-plugin consumer cursor)")
	}
	stream := cfg.StreamName
	if stream == "" {
		stream = StreamName
	}
	filters := cfg.Subjects
	if len(filters) == 0 {
		filters = []string{events.DefaultPrefix + ".>"}
	}
	ackWait := cfg.AckWait
	if ackWait <= 0 {
		ackWait = 30 * time.Second
	}

	// A plugin should keep trying to reach the bus rather than crash on a
	// transient NATS outage, mirroring the relay's reconnect-forever posture.
	opts := append([]nats.Option{
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.Name("ksquad-plugin-" + cfg.Durable),
	}, cfg.Options...)
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("jetstream.Subscribe: dial %s: %w", cfg.URL, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream.Subscribe: jetstream context: %w", err)
	}
	consumer, err := js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:        cfg.Durable,
		FilterSubjects: filters,
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        ackWait,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream.Subscribe: bind consumer %q on %s: %w", cfg.Durable, stream, err)
	}
	return &Subscriber{nc: nc, consumer: consumer}, nil
}

// Consume delivers each message to h until ctx is cancelled. A message whose
// subject does not parse into the five-token taxonomy is NAK'd (it is foreign to
// this stream's contract, so surfacing it beats silently acking it away). The
// call blocks; run it in its own goroutine.
func (s *Subscriber) Consume(ctx context.Context, h Handler) error {
	cc, err := s.consumer.Consume(func(m jetstream.Msg) {
		parts, perr := events.ParseSubject(m.Subject())
		if perr != nil {
			_ = m.Nak()
			return
		}
		msg := Message{SubjectParts: parts, Subject: m.Subject(), Payload: m.Data()}
		if herr := h(ctx, msg); herr != nil {
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return fmt.Errorf("jetstream.Consume: start: %w", err)
	}
	defer cc.Stop()
	<-ctx.Done()
	return ctx.Err()
}

// Close releases the NATS connection. The durable consumer is retained on the
// server so the plugin resumes from its cursor on the next Subscribe.
func (s *Subscriber) Close() {
	if s.nc != nil {
		s.nc.Close()
	}
}
