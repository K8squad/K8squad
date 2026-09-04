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

// This file is the pure, dependency-light core of story 13.8 (ISI-3724):
// resolving an OTelConfig SignalRouting into a normalized ExportTarget — the
// endpoint/protocol/auth-header/resource-attrs/sampler a signal exports with.
//
// It deliberately stops short of constructing a live OTLP exporter or touching
// any global provider: this code opens no network egress on its own. The
// exporter constructors (otlptrace{grpc,http}, otlpmetric*, otlplog*) and the
// wiring into telemetry.Setup are the *activation* step of 13.8, and that step
// is hard-gated on the 13.7 mandatory redaction processor (ISI-3723) sitting
// upstream of every exporter — §13.8 AC: opting into an external endpoint must
// never bypass PII/secret stripping. So the resolver lands now (fully tested,
// inert); the batcher→exporter wiring lands with 13.7 in place.
//
// Design: docs/bmad/stories/isi-3724-otelconfig-exporter-routing-reconcile.md
// (internal ksquad planning repo).
package telemetry

import (
	"fmt"
	"strings"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// authHeader is the export request header carrying the Secret-ref auth value.
// The value itself is resolved from a Secret at wiring time and is never logged.
const authHeader = "Authorization"

// ExportTarget is the normalized, transport-agnostic description of where and
// how one signal (traces/metrics/logs) is exported, derived from a
// SignalRouting. It is what the activation step feeds to the concrete OTLP
// exporter constructor for the routing's Protocol. Building it performs no I/O
// and starts no exporter.
type ExportTarget struct {
	// Endpoint is normalized per protocol: for grpc it is host[:port] with any
	// scheme stripped (the gRPC exporter takes a bare authority, not a URL);
	// for http/protobuf and http/json it is the full http(s) URL unchanged.
	Endpoint string

	// Protocol is the OTLP wire protocol (grpc | http/protobuf | http/json).
	Protocol ksquadv1alpha1.ExportProtocol

	// Insecure is true when a grpc endpoint was given an explicit http:// scheme
	// (or is a bare authority with no TLS implied) — the gRPC exporter must then
	// dial without transport credentials. For an https:// scheme it is false.
	// It is meaningless for the http/* protocols (the URL scheme carries it) and
	// left false there.
	Insecure bool

	// Headers are attached to every export request. When the routing has an
	// Auth ref and a resolved value is supplied, it carries {Authorization: …}.
	// Never logged.
	Headers map[string]string

	// ResourceAttrs are merged into the exported signal's OTel resource. A copy
	// of the routing's ResourceAttributes (nil when none).
	ResourceAttrs map[string]string

	// Sampler is the head sampler for the traces signal, derived from
	// Sampling. Nil when the routing sets no sampling (caller keeps the
	// provider default). Always nil for metrics/logs (sampling is traces-only,
	// enforced by CRD/webhook).
	Sampler sdktrace.Sampler
}

// TargetFromRouting resolves a SignalRouting into an ExportTarget. authValue is
// the value read from the routing's Auth Secret ref (empty when the routing has
// no Auth, or when the caller defers Secret resolution); when non-empty it is
// placed in the Authorization header. The function is pure: it validates and
// normalizes only, opening no connection and reading no Secret itself.
func TargetFromRouting(r *ksquadv1alpha1.SignalRouting, authValue string) (ExportTarget, error) {
	if r == nil {
		return ExportTarget{}, fmt.Errorf("telemetry: nil signal routing")
	}
	if strings.TrimSpace(r.Endpoint) == "" {
		return ExportTarget{}, fmt.Errorf("telemetry: routing endpoint is empty")
	}

	t := ExportTarget{Protocol: r.Protocol}

	switch r.Protocol {
	case ksquadv1alpha1.ExportProtocolGRPC:
		ep, insecure, err := normalizeGRPCEndpoint(r.Endpoint)
		if err != nil {
			return ExportTarget{}, err
		}
		t.Endpoint = ep
		t.Insecure = insecure
	case ksquadv1alpha1.ExportProtocolHTTPProtobuf, ksquadv1alpha1.ExportProtocolHTTPJSON:
		if !strings.HasPrefix(r.Endpoint, "http://") && !strings.HasPrefix(r.Endpoint, "https://") {
			return ExportTarget{}, fmt.Errorf("telemetry: %s endpoint must be a full http(s) URL, got %q", r.Protocol, r.Endpoint)
		}
		t.Endpoint = r.Endpoint
	default:
		return ExportTarget{}, fmt.Errorf("telemetry: unknown export protocol %q", r.Protocol)
	}

	if authValue != "" {
		t.Headers = map[string]string{authHeader: authValue}
	}

	if len(r.ResourceAttributes) > 0 {
		attrs := make(map[string]string, len(r.ResourceAttributes))
		for k, v := range r.ResourceAttributes {
			attrs[k] = v
		}
		t.ResourceAttrs = attrs
	}

	sampler, err := samplerFromSampling(r.Sampling)
	if err != nil {
		return ExportTarget{}, err
	}
	t.Sampler = sampler

	return t, nil
}

// normalizeGRPCEndpoint turns a grpc routing endpoint (host[:port] with an
// optional http(s) scheme, no path — as the CRD/webhook already enforce) into
// the bare authority the gRPC exporter expects, plus whether the dial is
// insecure. https:// ⇒ secure; http:// ⇒ insecure; no scheme ⇒ insecure (the
// operator opts into TLS explicitly with https://).
func normalizeGRPCEndpoint(endpoint string) (authority string, insecure bool, err error) {
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		authority, insecure = strings.TrimPrefix(endpoint, "https://"), false
	case strings.HasPrefix(endpoint, "http://"):
		authority, insecure = strings.TrimPrefix(endpoint, "http://"), true
	default:
		authority, insecure = endpoint, true
	}
	authority = strings.TrimSuffix(authority, "/")
	if authority == "" || strings.ContainsAny(authority, "/ ") {
		return "", false, fmt.Errorf("telemetry: grpc endpoint must be host[:port] with no path, got %q", endpoint)
	}
	return authority, insecure, nil
}

// samplerFromSampling maps a SamplingConfig to a head sampler. Nil config ⇒ nil
// sampler (caller keeps the provider default). Every sampler is ParentBased so
// a child span honors an upstream sampling decision (a Run trace continued from
// another process is not re-sampled at the head). probabilistic requires a
// ratio in (0,1] — mirrored from the CRD/webhook, re-checked here so a bad
// value fails closed rather than silently sampling everything.
func samplerFromSampling(s *ksquadv1alpha1.SamplingConfig) (sdktrace.Sampler, error) {
	if s == nil {
		return nil, nil
	}
	switch s.Type {
	case ksquadv1alpha1.SamplingTypeAlwaysOn:
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case ksquadv1alpha1.SamplingTypeAlwaysOff:
		return sdktrace.ParentBased(sdktrace.NeverSample()), nil
	case ksquadv1alpha1.SamplingTypeProbabilistic:
		if s.Ratio == nil || *s.Ratio <= 0 || *s.Ratio > 1 {
			return nil, fmt.Errorf("telemetry: probabilistic sampling.ratio must be in (0,1], got %v", s.Ratio)
		}
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(*s.Ratio)), nil
	default:
		return nil, fmt.Errorf("telemetry: unknown sampling type %q", s.Type)
	}
}
