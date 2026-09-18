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

package natstracing

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// SpanNATSConnect is the span name for establishing a NATS connection.
const SpanNATSConnect = "nats.connect"

// EventHandler handles one decoded A2A wire event with its traced context.
type EventHandler func(ctx context.Context, event wire.Event)

// NATSTelemetry provides NATS tracing integration with A2A events
type NATSTelemetry struct {
	tracer   trace.Tracer
	natsConn *nats.Conn
	mu       sync.RWMutex
}

// NewNATSTelemetry creates a new NATS telemetry instance
func NewNATSTelemetry() *NATSTelemetry {
	return &NATSTelemetry{
		tracer: Tracer(),
	}
}

// ConnectToNATS connects to NATS with tracing instrumentation
func (nt *NATSTelemetry) ConnectToNATS(ctx context.Context, url string) error {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	var err error
	nt.natsConn, err = nats.Connect(url,
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(5),
	)

	if err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}

	ctx, span := nt.tracer.Start(ctx, SpanNATSConnect,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String(keyMessagingSystem, messagingSystemNATS),
			attribute.String(keyMessagingDestination, url),
			attrComponent.String("nats"),
		),
	)
	defer span.End()

	if _, err := nt.natsConn.RTT(); err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("ping failed: %v", err))
		span.RecordError(err)
		return err
	}

	span.SetStatus(codes.Ok, "")
	_ = ctx
	return nil
}

// WrapConnection wraps an existing NATS connection with tracing
func (nt *NATSTelemetry) WrapConnection(nc *nats.Conn) *WrappedConn {
	return WrapConn(nc)
}

// PublishA2AEvent publishes an A2A event with tracing
func (nt *NATSTelemetry) PublishA2AEvent(ctx context.Context, subject string, event wire.Event) error {
	nt.mu.RLock()
	if nt.natsConn == nil {
		nt.mu.RUnlock()
		return fmt.Errorf("NATS connection not established")
	}
	nt.mu.RUnlock()

	wrappedConn := nt.WrapConnection(nt.natsConn)

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	return wrappedConn.PublishWithContext(ctx, subject, data)
}

// SubscribeToA2AEvents subscribes to A2A events with tracing
func (nt *NATSTelemetry) SubscribeToA2AEvents(ctx context.Context, subject string, handler EventHandler) (*nats.Subscription, error) {
	nt.mu.RLock()
	if nt.natsConn == nil {
		nt.mu.RUnlock()
		return nil, fmt.Errorf("NATS connection not established")
	}
	nt.mu.RUnlock()

	wrappedConn := nt.WrapConnection(nt.natsConn)

	return wrappedConn.SubscribeWithContext(ctx, subject, func(msg *nats.Msg) {
		msgCtx := ExtractTraceContext(msg)

		if !IsA2AMessage(msg) {
			return
		}

		var event wire.Event
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			fmt.Printf("Failed to unmarshal A2A event: %v\n", err)
			return
		}

		handler(msgCtx, event)
	})
}

// Close closes the NATS connection
func (nt *NATSTelemetry) Close() error {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	if nt.natsConn != nil {
		nt.natsConn.Close()
		nt.natsConn = nil
	}
	return nil
}

// IsConnected returns whether the NATS connection is established
func (nt *NATSTelemetry) IsConnected() bool {
	nt.mu.RLock()
	defer nt.mu.RUnlock()

	return nt.natsConn != nil && nt.natsConn.Status() == nats.CONNECTED
}

// GetStatus returns the current NATS connection status
func (nt *NATSTelemetry) GetStatus() string {
	nt.mu.RLock()
	defer nt.mu.RUnlock()

	if nt.natsConn == nil {
		return "disconnected"
	}
	return nt.natsConn.Status().String()
}
