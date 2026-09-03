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

// Package telemetry is the OpenTelemetry spine for the ksquad operator
// (ISI-2915 / ISI-3103). It installs four things once, at process start:
//
//   - a W3C trace-context propagator (+ baggage) — the on-the-wire format that
//     lets a Run's span join a trace that began in another process and lets a
//     downstream sandbox/agent continue this one;
//   - a TracerProvider — so every Run drive pass can open exactly one span
//     (see pkg/controller/rundrive), stitched into one distributed trace;
//   - a MeterProvider — so any code path can record a metric (bootstrap-path
//     counters/histograms in the ISI-3592 spec: ksquad.contextasm.* and
//     ksquad.taskio.*) without threading a meter through every call (ISI-3593);
//   - a LoggerProvider bridged into slog via contrib/bridges/otelslog — so any
//     structured log emitted with a span-bearing context automatically carries
//     trace_id/span_id, correlating logs to traces without a manual field.
//
// All three signals export to stdout by default ("stdout exporter is fine for
// now", ISI-3103 AC). The call sites — Tracer(), Meter(), Extract(), Inject(),
// and the slog logger — never name an exporter, so swapping stdout for OTLP
// later is a one-line change in Setup and nothing else moves.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	stdoutlog "go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	stdoutmetric "go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	stdouttrace "go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// instrumentationName is the tracer/logger scope for K8squad's own spans and
	// log records — the "who emitted this" name that shows up per instrumentation
	// scope in a backend.
	instrumentationName = "github.com/K8squad/K8squad"
	// defaultServiceName is the resource service.name when the caller leaves it
	// blank; a Run trace is only useful if the operator that drove it is named.
	defaultServiceName = "ksquad-operator"
)

// Options configures the telemetry spine. The zero value is valid and yields a
// stdout-exporting spine named "ksquad-operator".
type Options struct {
	// ServiceName is the resource service.name attached to every span and log
	// record. Empty defaults to "ksquad-operator".
	ServiceName string
	// Writer is where the stdout exporters emit. nil defaults to os.Stdout;
	// tests pass a buffer.
	Writer io.Writer
	// Pretty indents the stdout JSON. Convenient for local runs, noisy in prod.
	Pretty bool
}

// ShutdownFunc flushes and stops the trace, metric and log pipelines. Call it
// on process exit (with a bounded context) so buffered spans/metrics/logs are
// not lost.
type ShutdownFunc func(context.Context) error

// Setup installs the OpenTelemetry spine and returns the bridged *slog.Logger
// plus a shutdown that flushes both pipelines. It also registers the logger as
// slog's default, so slog.InfoContext(ctx, …) anywhere in the process is
// trace-correlated for free.
//
// Setup is not safe to call concurrently and is intended to run once from the
// operator entrypoint.
func Setup(ctx context.Context, opts Options) (*slog.Logger, ShutdownFunc, error) {
	if opts.ServiceName == "" {
		opts.ServiceName = defaultServiceName
	}
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}

	// One resource describes the emitting service for both signals, so a span
	// and its correlated log agree on service.name.
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", opts.ServiceName)))
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	// --- traces ---
	traceExpOpts := []stdouttrace.Option{stdouttrace.WithWriter(w)}
	if opts.Pretty {
		traceExpOpts = append(traceExpOpts, stdouttrace.WithPrettyPrint())
	}
	traceExp, err := stdouttrace.New(traceExpOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: stdout trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// W3C trace-context is the propagation format the ACs call for; baggage
	// rides alongside so future cross-cutting labels propagate too.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	// --- metrics ---
	// A PeriodicReader wraps the stdout exporter so instrument values are
	// collected and flushed on an interval (and on Shutdown). The default
	// interval is fine for a bootstrap-path spine; the reader export cadence,
	// like the exporter itself, is a one-line change when this moves to OTLP.
	metricExpOpts := []stdoutmetric.Option{stdoutmetric.WithWriter(w)}
	if opts.Pretty {
		metricExpOpts = append(metricExpOpts, stdoutmetric.WithPrettyPrint())
	}
	metricExp, err := stdoutmetric.New(metricExpOpts...)
	if err != nil {
		// Best-effort: don't leak the trace pipeline we already started.
		_ = tp.Shutdown(ctx)
		return nil, nil, fmt.Errorf("telemetry: stdout metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	// --- logs (otelslog bridge) ---
	logExpOpts := []stdoutlog.Option{stdoutlog.WithWriter(w)}
	if opts.Pretty {
		logExpOpts = append(logExpOpts, stdoutlog.WithPrettyPrint())
	}
	logExp, err := stdoutlog.New(logExpOpts...)
	if err != nil {
		// Best-effort: don't leak the trace and metric pipelines we already started.
		_ = errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
		return nil, nil, fmt.Errorf("telemetry: stdout log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	logglobal.SetLoggerProvider(lp)

	logger := otelslog.NewLogger(opts.ServiceName, otelslog.WithLoggerProvider(lp))
	slog.SetDefault(logger)

	shutdown := func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}
	return logger, shutdown, nil
}

// Tracer returns the process tracer for K8squad instrumentation. It reads the
// global TracerProvider on every call, so it is safe before Setup (a no-op
// tracer whose spans are dropped) and after (the real, exporting one). That is
// what lets pkg/controller/rundrive open a span unconditionally without a
// nil-check dance.
func Tracer() trace.Tracer {
	return otel.Tracer(instrumentationName)
}

// Meter returns the process meter for K8squad instrumentation. Like Tracer, it
// reads the global MeterProvider on every call, so it is safe before Setup (a
// no-op meter whose instruments record nothing) and after (the real, exporting
// one). That lets bootstrap-path code create its counters/histograms
// unconditionally — the ISI-3592 spec's ksquad.contextasm.* and ksquad.taskio.*
// instruments — without a nil-check dance.
func Meter() metric.Meter {
	return otel.Meter(instrumentationName)
}

// Extract lifts an inbound W3C trace context out of a string carrier — e.g. a
// Run's annotations stamped by whatever enqueued it — into ctx. A span started
// from the returned ctx becomes a child of the caller's distributed trace. An
// empty or absent carrier returns ctx unchanged, so the next span roots a fresh
// trace.
func Extract(ctx context.Context, carrier map[string]string) context.Context {
	if len(carrier) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(carrier))
}

// Inject writes ctx's current span context into carrier as W3C trace-context
// headers (traceparent/tracestate), so a downstream process — the sandbox that
// runs the agent — can Extract it and continue the same trace. carrier must be
// non-nil.
func Inject(ctx context.Context, carrier map[string]string) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(carrier))
}
