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

// Package natstracing provides OpenTelemetry instrumentation for NATS/JetStream
// operations, enabling end-to-end tracing of the message bus that connects
// operator→nats→supervisor→agent workflows.
package natstracing

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// Instrumentation name for NATS spans
	instrumentationName = "github.com/K8squad/K8squad/pkg/telemetry/natstracing"

	// Span names
	spanNATSConnect   = "nats.connect"
	spanNATSPublish   = "nats.publish"
	spanNATSSubscribe = "nats.subscribe"
	spanNATSStream    = "nats.stream"
	spanNATSConsumer  = "nats.consumer"
	spanNATSMessage   = "nats.message"

	// Messaging semantic-convention attribute keys (matching
	// pkg/events/jetstream/subscriber.go so both sides of the bus render
	// identically in the backend).
	keyMessagingSystem      = "messaging.system"
	keyMessagingOperation   = "messaging.operation"
	keyMessagingDestination = "messaging.destination.name"
	keyMessagingMessageID   = "messaging.message.id"

	messagingSystemNATS = "nats"

	// K8squad-specific attributes
	attrComponent     = attribute.Key("ksquad.component")
	attrMessageType   = attribute.Key("ksquad.message.type")
	attrMessageSource = attribute.Key("ksquad.message.source")
	attrMessageTarget = attribute.Key("ksquad.message.target")
	attrSubject       = attribute.Key("ksquad.subject")
	attrStreamName    = attribute.Key("ksquad.stream.name")
	attrConsumerName  = attribute.Key("ksquad.consumer.name")
)

// Tracer returns the NATS tracer
func Tracer() trace.Tracer {
	return otel.Tracer(instrumentationName)
}

// WrappedConn provides NATS connection with tracing instrumentation
type WrappedConn struct {
	*nats.Conn
	tracer trace.Tracer
}

// WrapConn wraps a NATS connection with tracing instrumentation
func WrapConn(nc *nats.Conn) *WrappedConn {
	return &WrappedConn{
		Conn:   nc,
		tracer: Tracer(),
	}
}

// PublishWithContext publishes a message with tracing. ISI-4540: the publish
// is a producer span AND the trace context is injected into the message
// headers, so the consumer side can continue the same distributed trace.
func (w *WrappedConn) PublishWithContext(ctx context.Context, subject string, data []byte) error {
	ctx, span := w.tracer.Start(ctx, spanNATSPublish,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String(keyMessagingSystem, messagingSystemNATS),
			attribute.String(keyMessagingOperation, "publish"),
			attribute.String(keyMessagingDestination, subject),
			attrSubject.String(subject),
			attrComponent.String("nats"),
		),
		trace.WithTimestamp(time.Now()),
	)
	defer span.End()

	msg := &nats.Msg{Subject: subject, Data: data, Header: make(nats.Header)}
	InjectTraceContext(ctx, msg)

	err := w.Conn.PublishMsg(msg)

	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("publish failed: %v", err))
		span.RecordError(err)
	}

	return err
}

// SubscribeWithContext creates a subscription with tracing
func (w *WrappedConn) SubscribeWithContext(ctx context.Context, subject string, handler nats.MsgHandler) (*nats.Subscription, error) {
	ctx, span := w.tracer.Start(ctx, spanNATSSubscribe,
		trace.WithAttributes(
			attribute.String(keyMessagingSystem, messagingSystemNATS),
			attribute.String(keyMessagingDestination, subject),
			attrSubject.String(subject),
			attrComponent.String("nats"),
		),
		trace.WithTimestamp(time.Now()),
	)
	defer span.End()

	sub, err := w.Conn.Subscribe(subject, func(msg *nats.Msg) {
		// ISI-4540: continue the producer's trace from the message headers so
		// the receive span is a child of the publish span, not a new root.
		msgCtx, msgSpan := w.tracer.Start(ExtractTraceContext(msg), spanNATSMessage,
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String(keyMessagingSystem, messagingSystemNATS),
				attribute.String(keyMessagingOperation, "receive"),
				attribute.String(keyMessagingDestination, msg.Subject),
				attrSubject.String(msg.Subject),
				attrMessageType.String("a2a.event"),
			),
		)
		_ = msgCtx

		handler(msg)

		msgSpan.End()
	})

	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("subscribe failed: %v", err))
		span.RecordError(err)
		return nil, err
	}

	return sub, nil
}

// JetStreamContext wraps a JetStream context with tracing
type JetStreamContext struct {
	nats.JetStreamContext
	tracer trace.Tracer
}

// WrapJetStream wraps a JetStream context with tracing instrumentation
func WrapJetStream(jsc nats.JetStreamContext, tracer trace.Tracer) *JetStreamContext {
	return &JetStreamContext{
		JetStreamContext: jsc,
		tracer:           tracer,
	}
}

// PublishStream publishes to a JetStream stream with tracing
func (j *JetStreamContext) PublishStream(ctx context.Context, stream string, subject string, data []byte) (*nats.PubAck, error) {
	ctx, span := j.tracer.Start(ctx, spanNATSStream,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String(keyMessagingSystem, messagingSystemNATS),
			attribute.String(keyMessagingOperation, "publish"),
			attribute.String(keyMessagingDestination, subject),
			attrStreamName.String(stream),
			attrSubject.String(subject),
			attrComponent.String("jetstream"),
		),
		trace.WithTimestamp(time.Now()),
	)
	defer span.End()

	msg := &nats.Msg{Subject: subject, Data: data, Header: make(nats.Header)}
	InjectTraceContext(ctx, msg)

	ack, err := j.JetStreamContext.PublishMsg(msg)

	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("stream publish failed: %v", err))
		span.RecordError(err)
	} else {
		span.SetAttributes(attribute.String(keyMessagingMessageID, strconv.FormatUint(ack.Sequence, 10)))
	}

	return ack, err
}

// CreateConsumer creates a JetStream consumer with tracing
func (j *JetStreamContext) CreateConsumer(ctx context.Context, stream string, config *nats.ConsumerConfig) (*nats.ConsumerInfo, error) {
	consumerName := ""
	if config != nil {
		consumerName = config.Name
	}
	ctx, span := j.tracer.Start(ctx, spanNATSConsumer,
		trace.WithAttributes(
			attribute.String(keyMessagingSystem, messagingSystemNATS),
			attribute.String(keyMessagingDestination, stream),
			attrStreamName.String(stream),
			attrConsumerName.String(consumerName),
			attrComponent.String("jetstream"),
		),
		trace.WithTimestamp(time.Now()),
	)
	defer span.End()

	info, err := j.JetStreamContext.AddConsumer(stream, config)

	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("consumer create failed: %v", err))
		span.RecordError(err)
	} else {
		span.SetAttributes(attrConsumerName.String(info.Name))
	}

	return info, err
}

// HeaderCarrier implements TextMapCarrier for NATS headers
type HeaderCarrier nats.Header

// Get implements TextMapCarrier
func (hc HeaderCarrier) Get(key string) string {
	return nats.Header(hc).Get(key)
}

// Set implements TextMapCarrier
func (hc HeaderCarrier) Set(key string, value string) {
	nats.Header(hc).Set(key, value)
}

// Keys implements TextMapCarrier
func (hc HeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(hc))
	for key := range hc {
		keys = append(keys, key)
	}
	return keys
}

// ExtractTraceContext extracts trace context from NATS message headers
func ExtractTraceContext(msg *nats.Msg) context.Context {
	if msg.Header == nil {
		return context.Background()
	}
	return otel.GetTextMapPropagator().Extract(context.Background(), HeaderCarrier(msg.Header))
}

// InjectTraceContext injects trace context into NATS message headers
func InjectTraceContext(ctx context.Context, msg *nats.Msg) {
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	otel.GetTextMapPropagator().Inject(ctx, HeaderCarrier(msg.Header))
}

// IsA2AMessage checks if a NATS message appears to be an A2A event
func IsA2AMessage(msg *nats.Msg) bool {
	subject := msg.Subject
	return strings.HasPrefix(subject, "a2a.") ||
		strings.Contains(subject, ".event") ||
		strings.Contains(subject, ".task")
}

// GetA2AMessageType extracts the A2A message type from subject
func GetA2AMessageType(subject string) string {
	if strings.HasPrefix(subject, "a2a.") {
		parts := strings.Split(subject, ".")
		if len(parts) >= 3 {
			return parts[2]
		}
	}
	return strings.ToLower(subject)
}
