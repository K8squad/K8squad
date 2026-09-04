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

package telemetry

import (
	"context"
	"testing"
)

func TestParseGRPCEndpoint(t *testing.T) {
	cases := []struct {
		name         string
		endpoint     string
		wantHostPort string
		wantInsecure bool
	}{
		{"no scheme is secure", "collector:4317", "collector:4317", false},
		{"https is secure", "https://collector:4317", "collector:4317", false},
		{"http is insecure", "http://collector:4317", "collector:4317", true},
		{"host only no port", "collector.ksquad-system.svc", "collector.ksquad-system.svc", false},
		{"http strips trailing path", "http://collector:4317/v1/traces", "collector:4317", true},
		{"https strips trailing path", "https://collector:4317/some/path", "collector:4317", false},
		{"no-scheme strips trailing path", "collector:4317/x", "collector:4317", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotHP, gotInsecure := parseGRPCEndpoint(tc.endpoint)
			if gotHP != tc.wantHostPort {
				t.Errorf("hostport = %q, want %q", gotHP, tc.wantHostPort)
			}
			if gotInsecure != tc.wantInsecure {
				t.Errorf("insecure = %v, want %v", gotInsecure, tc.wantInsecure)
			}
		})
	}
}

func TestSamplerFor(t *testing.T) {
	if got := samplerFor(nil); got != nil {
		t.Errorf("samplerFor(nil) = %v, want nil (caller omits WithSampler)", got)
	}
	cases := []struct {
		name string
		spec *SamplerSpec
	}{
		{"always_on", &SamplerSpec{Type: "always_on"}},
		{"always_off", &SamplerSpec{Type: "always_off"}},
		{"probabilistic", &SamplerSpec{Type: "probabilistic", Ratio: 0.25}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := samplerFor(tc.spec); got == nil {
				t.Errorf("samplerFor(%+v) = nil, want a sampler", tc.spec)
			}
		})
	}
	if got := samplerFor(&SamplerSpec{Type: "unknown"}); got != nil {
		t.Errorf("samplerFor(unknown) = %v, want nil", got)
	}
}

// TestBuildExportersOffline proves each build*Exporter constructs a usable
// exporter for every protocol WITHOUT dialing (the OTLP exporters connect
// lazily), so process start never blocks on an unreachable collector.
func TestBuildExportersOffline(t *testing.T) {
	ctx := context.Background()
	protocols := []struct {
		protocol string
		endpoint string
	}{
		{"grpc", "http://collector:4317"},
		{"http/protobuf", "http://collector:4318/v1/traces"},
		{"http/json", "http://collector:4318/v1/traces"},
	}
	for _, p := range protocols {
		t.Run("trace/"+p.protocol, func(t *testing.T) {
			exp, err := buildTraceExporter(ctx, &SignalExport{Protocol: p.protocol, Endpoint: p.endpoint})
			if err != nil || exp == nil {
				t.Fatalf("buildTraceExporter(%s) = (%v, %v)", p.protocol, exp, err)
			}
		})
		t.Run("metric/"+p.protocol, func(t *testing.T) {
			exp, err := buildMetricExporter(ctx, &SignalExport{Protocol: p.protocol, Endpoint: p.endpoint})
			if err != nil || exp == nil {
				t.Fatalf("buildMetricExporter(%s) = (%v, %v)", p.protocol, exp, err)
			}
		})
		t.Run("log/"+p.protocol, func(t *testing.T) {
			exp, err := buildLogExporter(ctx, &SignalExport{Protocol: p.protocol, Endpoint: p.endpoint})
			if err != nil || exp == nil {
				t.Fatalf("buildLogExporter(%s) = (%v, %v)", p.protocol, exp, err)
			}
		})
	}
}
