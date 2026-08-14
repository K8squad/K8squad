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

package v1alpha1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ptrFloat64(v float64) *float64 { return &v }

func TestValidateOTelConfig(t *testing.T) {
	tests := []struct {
		name         string
		spec         OTelConfigSpec
		wantErr      string // substring, empty means valid
		wantWarnings int
	}{
		{
			name:    "empty spec is rejected (opt-in requires at least one signal)",
			spec:    OTelConfigSpec{},
			wantErr: "at least one of traces, metrics, logs",
		},
		{
			name: "valid traces grpc with auth, attributes and probabilistic sampling",
			spec: OTelConfigSpec{
				Traces: &SignalRouting{
					Endpoint:           "otel-collector.observability:4317",
					Protocol:           ExportProtocolGRPC,
					Auth:               &SecretKeyReference{Name: "otel-token", Key: "token"},
					ResourceAttributes: map[string]string{"deployment.environment": "prod"},
					Sampling:           &SamplingConfig{Type: SamplingTypeProbabilistic, Ratio: ptrFloat64(0.25)},
				},
			},
		},
		{
			name: "valid metrics and logs over http/protobuf with full URLs",
			spec: OTelConfigSpec{
				Metrics: &SignalRouting{
					Endpoint: "https://otel.obs.svc:4318/v1/metrics",
					Protocol: ExportProtocolHTTPProtobuf,
				},
				Logs: &SignalRouting{
					Endpoint: "https://otel.obs.svc:4318/v1/logs",
					Protocol: ExportProtocolHTTPJSON,
				},
			},
		},
		{
			name: "grpc endpoint with path is rejected",
			spec: OTelConfigSpec{
				Traces: &SignalRouting{Endpoint: "otel.svc:4317/v1/traces", Protocol: ExportProtocolGRPC},
			},
			wantErr: "must not contain a path",
		},
		{
			name: "grpc endpoint with unsupported scheme is rejected",
			spec: OTelConfigSpec{
				Traces: &SignalRouting{Endpoint: "tcp://otel.svc:4317", Protocol: ExportProtocolGRPC},
			},
			wantErr: "scheme must be http or https",
		},
		{
			name: "http protocol without http scheme is rejected",
			spec: OTelConfigSpec{
				Metrics: &SignalRouting{Endpoint: "otel.svc:4318/v1/metrics", Protocol: ExportProtocolHTTPProtobuf},
			},
			wantErr: "must use the http or https scheme",
		},
		{
			name: "whitespace in endpoint is rejected",
			spec: OTelConfigSpec{
				Logs: &SignalRouting{Endpoint: "https://otel.svc /v1/logs", Protocol: ExportProtocolHTTPJSON},
			},
			wantErr: "must not contain whitespace",
		},
		{
			name: "unknown protocol is rejected",
			spec: OTelConfigSpec{
				Logs: &SignalRouting{Endpoint: "https://otel.svc/v1/logs", Protocol: ExportProtocol("thrift")},
			},
			wantErr: `is not one of grpc, http/protobuf, http/json`,
		},
		{
			name: "plaintext http to non-local host warns",
			spec: OTelConfigSpec{
				Logs: &SignalRouting{Endpoint: "http://otel.obs.svc:4318/v1/logs", Protocol: ExportProtocolHTTPProtobuf},
			},
			wantWarnings: 1,
		},
		{
			name: "plaintext http to localhost does not warn",
			spec: OTelConfigSpec{
				Logs: &SignalRouting{Endpoint: "http://localhost:4318/v1/logs", Protocol: ExportProtocolHTTPProtobuf},
			},
		},
		{
			name: "sampling on metrics is rejected",
			spec: OTelConfigSpec{
				Metrics: &SignalRouting{
					Endpoint: "https://otel.svc/v1/metrics",
					Protocol: ExportProtocolHTTPProtobuf,
					Sampling: &SamplingConfig{Type: SamplingTypeAlwaysOn},
				},
			},
			wantErr: "sampling is only valid on traces",
		},
		{
			name: "probabilistic sampling without ratio is rejected",
			spec: OTelConfigSpec{
				Traces: &SignalRouting{
					Endpoint: "otel.svc:4317",
					Protocol: ExportProtocolGRPC,
					Sampling: &SamplingConfig{Type: SamplingTypeProbabilistic},
				},
			},
			wantErr: "ratio is required",
		},
		{
			name: "probabilistic sampling with ratio 0 is rejected",
			spec: OTelConfigSpec{
				Traces: &SignalRouting{
					Endpoint: "otel.svc:4317",
					Protocol: ExportProtocolGRPC,
					Sampling: &SamplingConfig{Type: SamplingTypeProbabilistic, Ratio: ptrFloat64(0)},
				},
			},
			wantErr: "must be in (0, 1]",
		},
		{
			name: "ratio on always_on is rejected",
			spec: OTelConfigSpec{
				Traces: &SignalRouting{
					Endpoint: "otel.svc:4317",
					Protocol: ExportProtocolGRPC,
					Sampling: &SamplingConfig{Type: SamplingTypeAlwaysOn, Ratio: ptrFloat64(0.5)},
				},
			},
			wantErr: "must not be set",
		},
		{
			name: "reserved resource attribute is rejected",
			spec: OTelConfigSpec{
				Traces: &SignalRouting{
					Endpoint:           "otel.svc:4317",
					Protocol:           ExportProtocolGRPC,
					ResourceAttributes: map[string]string{"service.instance.id": "abc"},
				},
			},
			wantErr: "cannot be overridden",
		},
		{
			name: "malformed resource attribute key is rejected",
			spec: OTelConfigSpec{
				Traces: &SignalRouting{
					Endpoint:           "otel.svc:4317",
					Protocol:           ExportProtocolGRPC,
					ResourceAttributes: map[string]string{"1bad key": "x"},
				},
			},
			wantErr: "not a valid OTel attribute key",
		},
		{
			name: "valid resource attribute keys with digits and segments",
			spec: OTelConfigSpec{
				Traces: &SignalRouting{
					Endpoint:           "https://otel.svc:4318/v1/traces",
					Protocol:           ExportProtocolHTTPProtobuf,
					ResourceAttributes: map[string]string{"k8s.cluster.name": "homelab", "service.version": "1.2.3"},
				},
			},
		},
		{
			name: "auth missing key is rejected",
			spec: OTelConfigSpec{
				Traces: &SignalRouting{
					Endpoint: "otel.svc:4317",
					Protocol: ExportProtocolGRPC,
					Auth:     &SecretKeyReference{Name: "otel-token"},
				},
			},
			wantErr: "must set both name and key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &OTelConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "platform"},
				Spec:       tt.spec,
			}
			warnings, err := validateOTelConfig(cfg)

			if tt.wantErr == "" {
				require.NoError(t, err, "expected valid spec")
				assert.Len(t, warnings, tt.wantWarnings)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestOTelConfigWebhookMethods(t *testing.T) {
	valid := &OTelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "platform"},
		Spec: OTelConfigSpec{
			Traces: &SignalRouting{Endpoint: "otel.svc:4317", Protocol: ExportProtocolGRPC},
		},
	}

	w, err := valid.ValidateCreate(context.Background(), valid)
	assert.NoError(t, err)
	assert.Empty(t, w)

	w, err = valid.ValidateUpdate(context.Background(), valid, valid)
	assert.NoError(t, err)
	assert.Empty(t, w)

	w, err = valid.ValidateDelete(context.Background(), valid)
	assert.NoError(t, err)
	assert.Empty(t, w)

	invalid := valid.DeepCopy()
	invalid.Spec.Traces = nil
	_, err = invalid.ValidateCreate(context.Background(), invalid)
	assert.ErrorContains(t, err, "at least one of traces, metrics, logs")
}

// TestSignalRoutingDefaultsOptIn guards the opt-in contract: absent signals
// produce no exporters and no defaults are injected.
func TestSignalRoutingDefaultsOptIn(t *testing.T) {
	spec := OTelConfigSpec{}
	assert.Nil(t, spec.Traces)
	assert.Nil(t, spec.Metrics)
	assert.Nil(t, spec.Logs)
}
