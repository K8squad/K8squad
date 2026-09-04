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
	"testing"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

func ratio(f float64) *float64 { return &f }

func TestTargetFromRouting(t *testing.T) {
	tests := []struct {
		name         string
		routing      *ksquadv1alpha1.SignalRouting
		authValue    string
		wantEndpoint string
		wantInsecure bool
		wantAuth     bool
		wantSampler  bool
		wantErr      bool
	}{
		{
			name: "grpc bare authority is insecure",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "collector.obs.svc:4317",
				Protocol: ksquadv1alpha1.ExportProtocolGRPC,
			},
			wantEndpoint: "collector.obs.svc:4317",
			wantInsecure: true,
		},
		{
			name: "grpc https scheme is stripped and secure",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "https://otlp.dynatrace.com:443",
				Protocol: ksquadv1alpha1.ExportProtocolGRPC,
			},
			wantEndpoint: "otlp.dynatrace.com:443",
			wantInsecure: false,
		},
		{
			name: "grpc http scheme is stripped and insecure",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "http://collector:4317",
				Protocol: ksquadv1alpha1.ExportProtocolGRPC,
			},
			wantEndpoint: "collector:4317",
			wantInsecure: true,
		},
		{
			name: "grpc endpoint with a path is rejected",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "collector:4317/v1/traces",
				Protocol: ksquadv1alpha1.ExportProtocolGRPC,
			},
			wantErr: true,
		},
		{
			name: "http/protobuf keeps the full URL",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "https://otlp.example.com/v1/traces",
				Protocol: ksquadv1alpha1.ExportProtocolHTTPProtobuf,
			},
			wantEndpoint: "https://otlp.example.com/v1/traces",
		},
		{
			name: "http/json keeps the full URL",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "http://collector:4318/v1/metrics",
				Protocol: ksquadv1alpha1.ExportProtocolHTTPJSON,
			},
			wantEndpoint: "http://collector:4318/v1/metrics",
		},
		{
			name: "http/protobuf without a scheme is rejected",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "otlp.example.com/v1/traces",
				Protocol: ksquadv1alpha1.ExportProtocolHTTPProtobuf,
			},
			wantErr: true,
		},
		{
			name: "auth value becomes an Authorization header",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "https://otlp.example.com/v1/traces",
				Protocol: ksquadv1alpha1.ExportProtocolHTTPProtobuf,
			},
			authValue:    "Api-Token dt0c01.abc",
			wantEndpoint: "https://otlp.example.com/v1/traces",
			wantAuth:     true,
		},
		{
			name: "probabilistic sampling yields a sampler",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "collector:4317",
				Protocol: ksquadv1alpha1.ExportProtocolGRPC,
				Sampling: &ksquadv1alpha1.SamplingConfig{
					Type:  ksquadv1alpha1.SamplingTypeProbabilistic,
					Ratio: ratio(0.1),
				},
			},
			wantEndpoint: "collector:4317",
			wantInsecure: true,
			wantSampler:  true,
		},
		{
			name: "probabilistic sampling without ratio is rejected",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "collector:4317",
				Protocol: ksquadv1alpha1.ExportProtocolGRPC,
				Sampling: &ksquadv1alpha1.SamplingConfig{
					Type: ksquadv1alpha1.SamplingTypeProbabilistic,
				},
			},
			wantErr: true,
		},
		{
			name: "empty endpoint is rejected",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "   ",
				Protocol: ksquadv1alpha1.ExportProtocolGRPC,
			},
			wantErr: true,
		},
		{
			name: "unknown protocol is rejected",
			routing: &ksquadv1alpha1.SignalRouting{
				Endpoint: "collector:4317",
				Protocol: ksquadv1alpha1.ExportProtocol("thrift"),
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TargetFromRouting(tc.routing, tc.authValue)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got target %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Endpoint != tc.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", got.Endpoint, tc.wantEndpoint)
			}
			if got.Insecure != tc.wantInsecure {
				t.Errorf("insecure = %v, want %v", got.Insecure, tc.wantInsecure)
			}
			if _, hasAuth := got.Headers[authHeader]; hasAuth != tc.wantAuth {
				t.Errorf("has Authorization header = %v, want %v", hasAuth, tc.wantAuth)
			}
			if (got.Sampler != nil) != tc.wantSampler {
				t.Errorf("sampler present = %v, want %v", got.Sampler != nil, tc.wantSampler)
			}
		})
	}
}

func TestTargetFromRoutingNil(t *testing.T) {
	if _, err := TargetFromRouting(nil, ""); err == nil {
		t.Fatal("expected error for nil routing")
	}
}

func TestTargetFromRoutingCopiesResourceAttrs(t *testing.T) {
	src := map[string]string{"deployment.environment": "test"}
	r := &ksquadv1alpha1.SignalRouting{
		Endpoint:           "collector:4317",
		Protocol:           ksquadv1alpha1.ExportProtocolGRPC,
		ResourceAttributes: src,
	}
	got, err := TargetFromRouting(r, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got.ResourceAttrs["deployment.environment"] = "prod"
	if src["deployment.environment"] != "test" {
		t.Error("TargetFromRouting must copy ResourceAttributes, not alias the routing's map")
	}
}

func TestSamplerFromSampling(t *testing.T) {
	tests := []struct {
		name    string
		in      *ksquadv1alpha1.SamplingConfig
		wantNil bool
		wantErr bool
	}{
		{name: "nil config yields nil sampler", in: nil, wantNil: true},
		{name: "always_on", in: &ksquadv1alpha1.SamplingConfig{Type: ksquadv1alpha1.SamplingTypeAlwaysOn}},
		{name: "always_off", in: &ksquadv1alpha1.SamplingConfig{Type: ksquadv1alpha1.SamplingTypeAlwaysOff}},
		{name: "probabilistic valid", in: &ksquadv1alpha1.SamplingConfig{Type: ksquadv1alpha1.SamplingTypeProbabilistic, Ratio: ratio(0.5)}},
		{name: "probabilistic ratio 0 rejected", in: &ksquadv1alpha1.SamplingConfig{Type: ksquadv1alpha1.SamplingTypeProbabilistic, Ratio: ratio(0)}, wantErr: true},
		{name: "probabilistic ratio >1 rejected", in: &ksquadv1alpha1.SamplingConfig{Type: ksquadv1alpha1.SamplingTypeProbabilistic, Ratio: ratio(1.5)}, wantErr: true},
		{name: "unknown type rejected", in: &ksquadv1alpha1.SamplingConfig{Type: ksquadv1alpha1.SamplingType("tail")}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := samplerFromSampling(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if (got == nil) != tc.wantNil {
				t.Errorf("sampler nil = %v, want %v", got == nil, tc.wantNil)
			}
		})
	}
}
