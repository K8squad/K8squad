package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/telemetry"
)

// Waker delivers outbox NOTIFY wakeups to the relay. The production
// implementation (PgWaker, listen.go) is a LISTEN on `coord_outbox`; unit tests
// pass nil (poll-only) or a channel-backed fake. It only ever REDUCES latency —
// a missed wake is always caught by the next poll, so it is never relied on for
// delivery (that is what makes delivery at-least-once, C3).
type Waker interface {
	// Wakes returns a channel that receives (best-effort) when a new outbox row
	// is committed. It need not be reliable.
	Wakes() <-chan struct{}
	Close() error
}

// RelayConfig tunes the worker. Zero values are replaced with the defaults
// noted; only Store and Publisher are required.
type RelayConfig struct {
	Store     OutboxStore // REQUIRED: the outbox backlog view
	Publisher Publisher   // REQUIRED: the event bus
	Prefix    string      // subject root; "" ⇒ DefaultPrefix ("ksquad")
	Metrics   Metrics     // §17.2 signals; nil ⇒ no-op emitter
	Waker     Waker       // NOTIFY wake; nil ⇒ poll-only

	// PollInterval is the durable fallback scan cadence. The poll — not the
	// NOTIFY — is what guarantees at-least-once: every unflushed row is retried
	// each interval regardless of wakeups. "" ⇒ 2s.
	PollInterval time.Duration
	// BatchLimit caps rows scanned per flush (<=0 ⇒ unbounded). A cap keeps a
	// large backlog from starving the wake loop; the remainder is taken next tick.
	BatchLimit int
	// Logger is used for non-fatal publish failures; nil ⇒ slog.Default().
	Logger *slog.Logger
}

// Relay is the decoupled outbox → NATS worker (AC-b). It owns no coordination
// state and never participates in a write transaction; it only reads the outbox
// backlog, publishes, and stamps published_at. Constructing and running it can
// NOT affect a Run/claim/memory/scm write (C5) — nothing here is on the write
// path or the apiserver readiness probe.
type Relay struct {
	store   OutboxStore
	pub     Publisher
	prefix  string
	metrics Metrics
	waker   Waker
	poll    time.Duration
	batch   int
	log     *slog.Logger
}

// NewRelay validates cfg and returns a Relay. It errors on a missing Store or
// Publisher rather than nil-panicking mid-run.
func NewRelay(cfg RelayConfig) (*Relay, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("events.NewRelay: Store is required")
	}
	if cfg.Publisher == nil {
		return nil, fmt.Errorf("events.NewRelay: Publisher is required")
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	m := cfg.Metrics
	if m == nil {
		m = nopMetrics{}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Relay{
		store:   cfg.Store,
		pub:     cfg.Publisher,
		prefix:  prefix,
		metrics: m,
		waker:   cfg.Waker,
		poll:    poll,
		batch:   cfg.BatchLimit,
		log:     log,
	}, nil
}

// Run drives the relay until ctx is cancelled. It flushes once immediately (so a
// restart republishes the unflushed backlog — C3), then flushes on every NOTIFY
// wake and on every poll tick. Run only returns after ctx is done; a flush
// error is logged and retried on the next tick, never fatal (the write path must
// never depend on the relay making progress).
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()

	var wakes <-chan struct{}
	if r.waker != nil {
		wakes = r.waker.Wakes()
	}

	// Startup backlog drain: after any restart, every row still NULL is
	// republished — a row that failed to flush before the crash is retried.
	r.flushLogged(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.flushLogged(ctx)
		case <-wakes:
			r.flushLogged(ctx)
		}
	}
}

func (r *Relay) flushLogged(ctx context.Context) {
	if _, _, err := r.Flush(ctx); err != nil && ctx.Err() == nil {
		r.log.Warn("events relay flush error (will retry next tick)", "err", err)
	}
}

// Flush publishes every currently-unflushed outbox row (up to BatchLimit) in id
// order and stamps published_at on each success. It is safe to call directly
// (the tests do) and is the whole of the relay's behavior:
//
//   - subject is composed from the row COLUMNS, never the payload (C2);
//   - published_at is stamped ONLY after a successful, acked publish, so a row
//     is never marked for an event that did not reach the bus (C2 set-once);
//   - a publish failure increments publish_failures, leaves the row NULL, and
//     does NOT abort the batch — the row is retried next tick / after restart,
//     so NATS-down yields buffering, never loss (C3, at-least-once);
//   - the §17.2 depth/unflushed/consumer-lag gauges are refreshed each call.
//
// It returns (published, failed) counts for the caller/tests.
func (r *Relay) Flush(ctx context.Context) (published, failed int, err error) {
	rows, err := r.store.Unpublished(ctx, r.batch)
	if err != nil {
		return 0, 0, err
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		subject := Subject(r.prefix, row.Entity, row.ProjectID, row.Squad, row.EventType)
		if perr := r.publish(ctx, subject, row); perr != nil {
			// AT-LEAST-ONCE: do NOT stamp — the row stays NULL and is retried.
			failed++
			r.metrics.IncPublishFailures()
			r.log.Debug("outbox publish failed (row retained for retry)",
				"id", row.ID, "subject", subject, "err", perr)
			continue
		}
		if merr := r.store.MarkPublished(ctx, row.ID); merr != nil {
			// The event IS on the bus but we failed to record the flush. Leaving
			// it NULL means one at-least-once redelivery on the next tick — the
			// correct, safe outcome (subscribers must be idempotent), never loss.
			failed++
			r.log.Warn("outbox row published but mark-published failed (will redeliver once)",
				"id", row.ID, "err", merr)
			continue
		}
		published++
	}
	r.refreshMetrics(ctx)
	return published, failed, nil
}

// publish emits one outbox row to the bus INSIDE a producer span that joins the
// run trace (ISI-4440). It restores the run's span context from the row, opens a
// `nats publish <subject>` span (OTel messaging semconv), injects the resulting
// carrier into the message headers, and prefers the HeaderPublisher path so the
// consumer can continue the same trace; a Publisher without that capability
// falls back to the header-less Publish (the event still flows, untraced). The
// span records the publish outcome and always ends — a NATS-down error is
// surfaced to the caller unchanged so the at-least-once retry is untouched.
func (r *Relay) publish(ctx context.Context, subject string, row OutboxRow) error {
	spanCtx, span := telemetry.Tracer().Start(
		runTraceContext(ctx, row),
		"nats publish "+subject,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(messagingAttrs("publish", subject, row.RunID)...),
	)
	defer span.End()

	hp, ok := r.pub.(HeaderPublisher)
	if !ok {
		// No header capability: publish untraced. Do it under the row's original
		// ctx (not spanCtx) so no half-propagated context leaks downstream.
		err := r.pub.Publish(ctx, subject, row.Payload)
		recordPublishSpan(span, err)
		return err
	}
	headers := injectCarrier(spanCtx)
	err := hp.PublishMsg(spanCtx, subject, row.Payload, headers)
	recordPublishSpan(span, err)
	return err
}

// recordPublishSpan stamps a producer span's status from the publish outcome.
func recordPublishSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "nats publish failed")
	}
}

// refreshMetrics updates the depth/unflushed gauges and, when the Publisher can
// report it, the JetStream consumer-lag gauge. Best-effort: a metrics read error
// never fails a flush.
func (r *Relay) refreshMetrics(ctx context.Context) {
	if total, unflushed, err := r.store.Depth(ctx); err == nil {
		r.metrics.SetDepth(total, unflushed)
	}
	if lr, ok := r.pub.(LagReporter); ok {
		if pending, err := lr.ConsumerLag(ctx); err == nil {
			r.metrics.SetConsumerLag(pending)
		}
	}
}
