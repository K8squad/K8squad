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
// All three signals export to stdout by DEFAULT ("stdout exporter is fine for
// now", ISI-3103 AC). Per-signal OTLP export is OPT-IN (ISI-3620): each of
// Options.Traces/Metrics/Logs is a *SignalExport describing one signal's OTLP
// target; a nil *SignalExport keeps that signal on the stdout default. Those
// exports are sourced, once at process start, from the cluster-scoped OTelConfig
// CR by the kube-aware pkg/telemetry/otelcr layer — this package stays a pure,
// kube-free spine. The call sites — Tracer(), Meter(), Extract(), Inject(), and
// the slog logger — never name an exporter, honoring the original "swap is a
// one-line change" intent: the selection is exactly one line per signal in Setup
// and nothing else moves.
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
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
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
	// defaultRepositoryURL is the resource vcs.repository.url.full when the
	// caller leaves it blank (P3#8 / ISI-5016): every span's service must map
	// to its repository deterministically instead of by name resemblance.
	defaultRepositoryURL = "https://github.com/K8squad/K8squad"
)

// propagator is the W3C trace-context + baggage propagator used by Extract and
// Inject. It is initialized at package load — NOT inside Setup — because the
// sandbox entrypoints (`shim run` / `shim supervisor`) call Extract to continue
// the operator-injected run trace BEFORE they install the telemetry spine via
// Setup (main.go / supervisor.go extract, then Setup with the extracted ctx so
// the resource-detecting Setup does not lose the parent). When Extract/Inject
// read otel.GetTextMapPropagator() they got the SDK-default *no-op* propagator
// at that point, so the inbound traceparent was silently discarded and every
// sandbox span rooted its own trace — the "single-span / disconnected" traces
// Henrik reported (ISI-4540, ISI-4413). Using a package-level propagator makes
// extraction independent of Setup ordering. Setup still registers this SAME
// value as the OTel global so third-party instrumentation shares the format.
var propagator propagation.TextMapPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{}, propagation.Baggage{})

// SamplerSpec is a neutral, kube-free description of a head sampler (traces
// only).
type SamplerSpec struct {
	Type  string  // "always_on" | "always_off" | "probabilistic"
	Ratio float64 // fraction kept when Type == "probabilistic"; ignored otherwise
}

// SignalExport describes an OTLP export target for ONE signal. A nil
// *SignalExport on Options means: keep the stdout default for that signal
// (opt-in, C-AC2). Headers carry already-resolved auth (e.g. "Authorization")
// and are NEVER logged.
type SignalExport struct {
	Protocol string // "grpc" | "http/protobuf" | "http/json"
	Endpoint string
	Headers  map[string]string
	Sampler  *SamplerSpec // traces only; ignored on metrics/logs
}

// Options configures the telemetry spine. The zero value is valid and yields a
// stdout-exporting spine named "ksquad-operator".
type Options struct {
	// ServiceName is the resource service.name attached to every span and log
	// record. Empty defaults to "ksquad-operator".
	ServiceName string
	// RepositoryURL is the resource vcs.repository.url.full attached to every
	// span and log record, mapping the emitting service to its source
	// repository (P3#8 / ISI-5016). Empty defaults to
	// "https://github.com/K8squad/K8squad".
	RepositoryURL string
	// Writer is where the stdout exporters emit. nil defaults to os.Stdout;
	// tests pass a buffer.
	Writer io.Writer
	// Pretty indents the stdout JSON. Convenient for local runs, noisy in prod.
	Pretty bool
	// Traces, Metrics, Logs each opt one signal into OTLP export (ISI-3620). A
	// nil *SignalExport keeps that signal on the stdout default (C-AC2). These
	// are populated once at process start from the OTelConfig CR by the
	// pkg/telemetry/otelcr layer; this package never touches kube.
	Traces  *SignalExport
	Metrics *SignalExport
	Logs    *SignalExport

	// CaptureUnsampledRemoteParent forces the traces head sampler to RECORD a
	// span even when it continues a REMOTE parent whose W3C sampled flag is 0,
	// while still honoring every other case (a sampled remote parent, a local
	// parent, or no parent at all). It exists for the sandbox entrypoints
	// (`shim run` / `shim supervisor`, ISI-4413): a sandbox continues the Run's
	// distributed trace from the traceparent the operator injected into the
	// run credential, so the run's spans (run.start / llm.call /
	// gen_ai.tool.call / run.end) inherit that parent's sampling decision under
	// the SDK-default ParentBased(AlwaysSample) sampler. When the injected
	// parent carries sampled=0 (e.g. the bind-pass reconcile span the credential
	// froze was itself dropped, or the operator samples probabilistically), the
	// WHOLE run subtree is head-dropped in the sandbox and never reaches the
	// collector — Henrik's "disconnected span, without run.start / run.end"
	// (ISI-4365). A sandbox hosts exactly one Run, so its run trace must be
	// captured unconditionally; this flag makes the remote-parent-not-sampled
	// case sample-anyway without re-sampling any other case and without breaking
	// dispatcher→run parenting (the span still uses the injected trace/parent
	// id). Ignored when an explicit CR sampler is set (declared routing wins).
	CaptureUnsampledRemoteParent bool
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
	if opts.RepositoryURL == "" {
		opts.RepositoryURL = defaultRepositoryURL
	}
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}

	// One resource describes the emitting service for both signals, so a span
	// and its correlated log agree on service.name. vcs.repository.url.full
	// (P3#8 / ISI-5016) additionally pins every span's service to its source
	// repository, so a backend maps service↔repo deterministically rather than
	// guessing by name resemblance.
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(
			attribute.String("service.name", opts.ServiceName),
			semconv.VCSRepositoryURLFull(opts.RepositoryURL),
		))
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	// --- traces ---
	// Per-signal selection (ISI-3620): a nil opts.Traces keeps the stdout
	// default; a non-nil one swaps in the OTLP exporter (one line, as promised).
	var traceExp sdktrace.SpanExporter
	if opts.Traces == nil {
		traceExpOpts := []stdouttrace.Option{stdouttrace.WithWriter(w)}
		if opts.Pretty {
			traceExpOpts = append(traceExpOpts, stdouttrace.WithPrettyPrint())
		}
		traceExp, err = stdouttrace.New(traceExpOpts...)
	} else {
		traceExp, err = buildTraceExporter(ctx, opts.Traces)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: trace exporter: %w", err)
	}
	traceProviderOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	}
	// Head sampler selection:
	//   - CaptureUnsampledRemoteParent (sandbox path, ISI-4413) COMPOSES with the
	//     CR-declared root decision instead of being suppressed by it: it keeps
	//     whatever the CR spec chooses for a fresh ROOT span (or AlwaysSample when
	//     no CR sampler is set), but ALWAYS captures a remote parent whose sampled
	//     flag is 0. A sandbox hosts exactly one Run, so its run trace must be
	//     recorded regardless of the injected credential traceparent's decision —
	//     and that safety net must not silently switch off for deployments that
	//     configure a CR sampler (ISI-4540 review).
	//   - otherwise the CR-declared sampler (opts.Traces.Sampler) wins as before,
	//     and a nil spec leaves the SDK default untouched.
	var traceSampler *SamplerSpec
	if opts.Traces != nil {
		traceSampler = opts.Traces.Sampler
	}
	var sampler sdktrace.Sampler
	if opts.CaptureUnsampledRemoteParent {
		root := rootSamplerFor(traceSampler)
		if root == nil {
			root = sdktrace.AlwaysSample()
		}
		sampler = sdktrace.ParentBased(root,
			sdktrace.WithRemoteParentNotSampled(sdktrace.AlwaysSample()))
	} else {
		sampler = samplerFor(traceSampler)
	}
	if sampler != nil {
		traceProviderOpts = append(traceProviderOpts, sdktrace.WithSampler(sampler))
	}
	tp := sdktrace.NewTracerProvider(traceProviderOpts...)
	otel.SetTracerProvider(tp)

	// W3C trace-context is the propagation format the ACs call for; baggage
	// rides alongside so future cross-cutting labels propagate too. Register the
	// same package-level propagator Extract/Inject already use, so global-reading
	// third-party instrumentation and this package's own helpers never disagree.
	otel.SetTextMapPropagator(propagator)

	// --- metrics ---
	// A PeriodicReader wraps the stdout exporter so instrument values are
	// collected and flushed on an interval (and on Shutdown). The default
	// interval is fine for a bootstrap-path spine; the reader export cadence,
	// like the exporter itself, is a one-line change when this moves to OTLP.
	var metricExp sdkmetric.Exporter
	if opts.Metrics == nil {
		metricExpOpts := []stdoutmetric.Option{stdoutmetric.WithWriter(w)}
		if opts.Pretty {
			metricExpOpts = append(metricExpOpts, stdoutmetric.WithPrettyPrint())
		}
		metricExp, err = stdoutmetric.New(metricExpOpts...)
	} else {
		metricExp, err = buildMetricExporter(ctx, opts.Metrics)
	}
	if err != nil {
		// Best-effort: don't leak the trace pipeline we already started.
		_ = tp.Shutdown(ctx)
		return nil, nil, fmt.Errorf("telemetry: metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	// --- logs (otelslog bridge) ---
	var logExp sdklog.Exporter
	if opts.Logs == nil {
		logExpOpts := []stdoutlog.Option{stdoutlog.WithWriter(w)}
		if opts.Pretty {
			logExpOpts = append(logExpOpts, stdoutlog.WithPrettyPrint())
		}
		logExp, err = stdoutlog.New(logExpOpts...)
	} else {
		logExp, err = buildLogExporter(ctx, opts.Logs)
	}
	if err != nil {
		// Best-effort: don't leak the trace and metric pipelines we already started.
		_ = errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
		return nil, nil, fmt.Errorf("telemetry: log exporter: %w", err)
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
	// Use the package-level propagator, not otel.GetTextMapPropagator(): the
	// sandbox entrypoints call Extract before Setup installs the global, and the
	// default global is a no-op that would drop the inbound traceparent.
	out := propagator.Extract(ctx, propagation.MapCarrier(carrier))
	// Self-diagnosing (ISI-4540): the propagator can no longer be the cause of a
	// dropped parent, but the SAME symptom still arrives from a missing/malformed
	// traceparent (an injector that didn't run, a truncated value). A carrier that
	// carries a traceparent yet yields no valid parent means exactly that, and the
	// run will silently root its own trace. Surface it on stderr (both sandbox
	// entrypoints already log telemetry diagnostics there, so no stdout-corruption
	// risk) so the next recurrence is not invisible.
	if _, hasTraceparent := carrier["traceparent"]; hasTraceparent &&
		!trace.SpanContextFromContext(out).IsValid() {
		fmt.Fprintf(os.Stderr, "telemetry: carrier has a traceparent but yielded no valid "+
			"parent span context (malformed/dropped; run will root its own trace); carrier keys=%v\n",
			keysOf(carrier))
	}
	return out
}

// Inject writes ctx's current span context into carrier as W3C trace-context
// headers (traceparent/tracestate), so a downstream process — the sandbox that
// runs the agent — can Extract it and continue the same trace. carrier must be
// non-nil.
func Inject(ctx context.Context, carrier map[string]string) {
	// Package-level propagator (see Extract): keeps Inject order-independent of
	// Setup so an operator that injects before installing the global still emits
	// a valid traceparent.
	propagator.Inject(ctx, propagation.MapCarrier(carrier))
}

// keysOf returns a carrier's keys for diagnostics — never the values, which can
// carry trace/tracestate/baggage content we do not want on stderr.
func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
