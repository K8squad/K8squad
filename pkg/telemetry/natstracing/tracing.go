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
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

const (
	// Instrumentation name for NATS spans
	instrumentationName = "github.com/K8squad/K8squad/pkg/telemetry/natstracing"
	
	// Span names
	spanNATSConnect      = "nats.connect"
	spanNATSPublish      = "nats.publish"
	spanNATSSubscribe    = "nats.subscribe"
	spanNATSStream       = "nats.stream"
	spanNATSConsumer     = "nats.consumer"
	spanNATSMessage      = "nats.message"
	
	// K8squad-specific attributes
	attrComponent     = attribute.Key("ksquad.component")
	attrMessageType   = attribute.Key("ksquad.message.type")
	attrMessageSource = attribute.Key("ksquad.message.source")
	attrMessageTarget = attribute.Key("ksquad.message.target")
	attrSubject       = attribute.Key("ksquad.subject")
	attrStreamName    = attribute.Key("ksquad.stream.name")
	attrConsumerName   = attribute.Key("ksquad.consumer.name")
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

// PublishWithContext publishes a message with tracing
func (w *WrappedConn) PublishWithContext(ctx context.Context, subject string, data []byte) error {
	ctx, span := w.tracer.Start(ctx, spanNATSPublish,
		trace.WithAttributes(
			semconv.MessagingSystemNATS,
			semconv.MessagingDestinationKindTopic.String(subject),
			attrSubject.String(subject),
		),
		trace.WithTimestamp(time.Now()),
	)
	defer span.End()
	
	err := w.Conn.Publish(subject, data)
	
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("publish failed: %v", err))
		span.RecordError(err)
	}
	
	span.SetAttributes(attrComponent.String("nats"))
	span.SetAttributes(attrMessageSource.String("operator"))
	
	return err
}

// SubscribeWithContext creates a subscription with tracing
func (w *WrappedConn) SubscribeWithContext(ctx context.Context, subject string, handler nats.MsgHandler) (*nats.Subscription, error) {
	ctx, span := w.tracer.Start(ctx, spanNATSSubscribe,
		trace.WithAttributes(
			semconv.MessagingSystemNATS,
			semconv.MessagingDestinationKindTopic.String(subject),
			attrSubject.String(subject),
		),
		trace.WithTimestamp(time.Now()),
	)
	defer span.End()
	
	sub, err := w.Conn.Subscribe(subject, func(msg *nats.Msg) {
		// Create a span for each message received
		msgCtx, msgSpan := w.tracer.Start(msg.Context, spanNATSMessage,
			trace.WithAttributes(
				semconv.MessagingSystemNATS,
				semconv.MessagingDestinationKindTopic.String(msg.Subject),
				semconv.MessagingMessageID.String(msg.Reply),
				attrSubject.String(msg.Subject),
				attrMessageType.String("a2a.event"),
				attrMessageSource.String("operator"),
				attrMessageTarget.String("supervisor"),
			),
		)
		
		// Attach trace context to the message if not already present
		if msg.Header == nil {
			msg.Header = make(nats.Header)
		}
		otel.GetTextMapPropagator().Inject(msgCtx, nats.HeaderCarrier(msg.Header))
		
		// Call the original handler with traced context
		handler(msg)
		
		msgSpan.End()
	})
	
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("subscribe failed: %v", err))
		span.RecordError(err)
		return nil, err
	}
	
	span.SetAttributes(attrComponent.String("nats"))
	span.SetAttributes(attrMessageTarget.String("supervisor"))
	
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
		tracer:          tracer,
	}
}

// PublishStream publishes to a JetStream stream with tracing
func (j *JetStreamContext) PublishStream(ctx context.Context, stream string, subject string, data []byte) (*nats.PubAck, error) {
	ctx, span := j.tracer.Start(ctx, spanNATSStream,
		trace.WithAttributes(
			semconv.MessagingSystemNATS,
			semconv.MessagingDestinationKindTopic.String(stream),
			attrStreamName.String(stream),
			attrSubject.String(subject),
		),
		trace.WithTimestamp(time.Now()),
	)
	defer span.End()
	
	ack, err := j.JetStreamContext.Publish(subject, data)
	
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("stream publish failed: %v", err))
		span.RecordError(err)
	} else {
		span.SetAttributes(semconv.MessagingMessageID.String(ack.StreamSequence))
	}
	
	span.SetAttributes(attrComponent.String("jetstream"))
	span.SetAttributes(attrMessageSource.String("operator"))
	
	return ack, err
}

// CreateConsumer creates a JetStream consumer with tracing
func (j *JetStreamContext) CreateConsumer(ctx context.Context, stream string, config *nats.ConsumerConfig) (*nats.ConsumerInfo, error) {
	ctx, span := j.tracer.Start(ctx, spanNATSConsumer,
		trace.WithAttributes(
			semconv.MessagingSystemNATS,
			semconv.MessagingDestinationKindTopic.String(stream),
			attrStreamName.String(stream),
			attrConsumerName.String(config.Name),
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
	
	span.SetAttributes(attrComponent.String("jetstream"))
	span.SetAttributes(attrMessageTarget.String("supervisor"))
	
	return info, err
}

// HeaderCarrier implements TextMapCarrier for NATS headers
type HeaderCarrier nats.Header

// Get implements TextMapCarrier
func (hc HeaderCarrier) Get(key string) string {
	return string(hc[key])
}

// Set implements TextMapCarrier
func (hc HeaderCarrier) Set(key string, value string) {
	hc[key] = []byte(value)
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
	return otel.GetTextMapPropagator().Extract(msg.Context, HeaderCarrier(msg.Header))
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