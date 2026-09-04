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

// otlp.go holds the pure, kube-free OTLP exporter constructors the spine swaps
// in when a signal is routed by the OTelConfig CR (ISI-3620). It imports only
// the OTel exporter SDKs — no api/v1alpha1, no kube — so it stays a leaf of the
// neutral spine. The constructors do NOT dial: the OTLP exporters connect
// lazily on first export, so building one against an unreachable endpoint
// succeeds and never blocks process start.
package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// parseGRPCEndpoint strips an optional scheme off a gRPC endpoint and reports
// whether the connection should be insecure (plaintext). Rules:
//   - "https://" or no scheme -> insecure=false (default TLS);
//   - "http://" -> insecure=true.
//
// The returned hostport has the scheme stripped and any trailing path removed,
// so it is suitable for otlp*grpc.WithEndpoint (which wants host:port only).
func parseGRPCEndpoint(endpoint string) (hostport string, insecure bool) {
	hostport = endpoint
	switch {
	case strings.HasPrefix(hostport, "https://"):
		hostport = strings.TrimPrefix(hostport, "https://")
		insecure = false
	case strings.HasPrefix(hostport, "http://"):
		hostport = strings.TrimPrefix(hostport, "http://")
		insecure = true
	default:
		insecure = false
	}
	// A gRPC endpoint is host:port; drop any accidental trailing path.
	if i := strings.IndexByte(hostport, '/'); i >= 0 {
		hostport = hostport[:i]
	}
	return hostport, insecure
}

// samplerFor maps a neutral SamplerSpec onto an SDK head sampler. A nil spec
// returns nil so the caller omits WithSampler and keeps the SDK default.
func samplerFor(s *SamplerSpec) sdktrace.Sampler {
	if s == nil {
		return nil
	}
	switch s.Type {
	case "always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	case "always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample())
	case "probabilistic":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(s.Ratio))
	default:
		return nil
	}
}

// buildTraceExporter constructs an OTLP span exporter for se. Protocol dispatch:
// "grpc" -> the grpc exporter; "http/protobuf" and "http/json" -> the http
// exporter.
//
// W1: the Go OTLP/HTTP exporters only encode protobuf, so "http/json" also uses
// the protobuf http exporter — a JSON body is not actually emitted. This v1
// limitation means http/json is reachable only via a direct CR edit and is
// accepted for v1.
func buildTraceExporter(ctx context.Context, se *SignalExport) (sdktrace.SpanExporter, error) {
	if se.Protocol == "grpc" {
		hostport, insecure := parseGRPCEndpoint(se.Endpoint)
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(hostport)}
		if insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		if len(se.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(se.Headers))
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(se.Endpoint)}
	if len(se.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(se.Headers))
	}
	return otlptracehttp.New(ctx, opts...)
}

// buildMetricExporter constructs an OTLP metric exporter for se (see
// buildTraceExporter for protocol dispatch and the W1 http/json note).
func buildMetricExporter(ctx context.Context, se *SignalExport) (sdkmetric.Exporter, error) {
	if se.Protocol == "grpc" {
		hostport, insecure := parseGRPCEndpoint(se.Endpoint)
		opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(hostport)}
		if insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		if len(se.Headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(se.Headers))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	}
	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(se.Endpoint)}
	if len(se.Headers) > 0 {
		opts = append(opts, otlpmetrichttp.WithHeaders(se.Headers))
	}
	return otlpmetrichttp.New(ctx, opts...)
}

// buildLogExporter constructs an OTLP log exporter for se (see
// buildTraceExporter for protocol dispatch and the W1 http/json note).
func buildLogExporter(ctx context.Context, se *SignalExport) (sdklog.Exporter, error) {
	if se.Protocol == "grpc" {
		hostport, insecure := parseGRPCEndpoint(se.Endpoint)
		opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(hostport)}
		if insecure {
			opts = append(opts, otlploggrpc.WithInsecure())
		}
		if len(se.Headers) > 0 {
			opts = append(opts, otlploggrpc.WithHeaders(se.Headers))
		}
		return otlploggrpc.New(ctx, opts...)
	}
	opts := []otlploghttp.Option{otlploghttp.WithEndpointURL(se.Endpoint)}
	if len(se.Headers) > 0 {
		opts = append(opts, otlploghttp.WithHeaders(se.Headers))
	}
	return otlploghttp.New(ctx, opts...)
}
