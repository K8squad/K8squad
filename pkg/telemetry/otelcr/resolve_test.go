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

package otelcr

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

const systemNS = "ksquad-system"

// fakeSecrets is an in-memory SecretGetter keyed by "namespace/name/key".
type fakeSecrets map[string][]byte

func (f fakeSecrets) Get(_ context.Context, namespace, name, key string) ([]byte, error) {
	if v, ok := f[namespace+"/"+name+"/"+key]; ok {
		return v, nil
	}
	return nil, errors.New("secret or key not found")
}

func float64Ptr(f float64) *float64 { return &f }

func TestResolveMapsProtocolEndpointPerSignal(t *testing.T) {
	cr := &ksquadv1.OTelConfig{
		Spec: ksquadv1.OTelConfigSpec{
			Traces:  &ksquadv1.SignalRouting{Endpoint: "collector:4317", Protocol: ksquadv1.ExportProtocolGRPC},
			Metrics: &ksquadv1.SignalRouting{Endpoint: "https://c:4318/v1/metrics", Protocol: ksquadv1.ExportProtocolHTTPProtobuf},
			Logs:    &ksquadv1.SignalRouting{Endpoint: "https://c:4318/v1/logs", Protocol: ksquadv1.ExportProtocolHTTPJSON},
		},
	}
	res := Resolve(context.Background(), cr, fakeSecrets{}, systemNS)
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if res.Traces == nil || res.Traces.Protocol != "grpc" || res.Traces.Endpoint != "collector:4317" {
		t.Errorf("traces = %+v", res.Traces)
	}
	if res.Metrics == nil || res.Metrics.Protocol != "http/protobuf" || res.Metrics.Endpoint != "https://c:4318/v1/metrics" {
		t.Errorf("metrics = %+v", res.Metrics)
	}
	if res.Logs == nil || res.Logs.Protocol != "http/json" || res.Logs.Endpoint != "https://c:4318/v1/logs" {
		t.Errorf("logs = %+v", res.Logs)
	}
}

func TestResolveNilCRAndAbsentSignals(t *testing.T) {
	if res := Resolve(context.Background(), nil, fakeSecrets{}, systemNS); res.Traces != nil || res.Metrics != nil || res.Logs != nil || len(res.Errors) != 0 {
		t.Fatalf("nil CR should resolve to all-stdout empty: %+v", res)
	}
	// A CR with only metrics: traces & logs stay nil (stdout).
	cr := &ksquadv1.OTelConfig{Spec: ksquadv1.OTelConfigSpec{
		Metrics: &ksquadv1.SignalRouting{Endpoint: "c:4317", Protocol: ksquadv1.ExportProtocolGRPC},
	}}
	res := Resolve(context.Background(), cr, fakeSecrets{}, systemNS)
	if res.Traces != nil || res.Logs != nil {
		t.Errorf("absent signals should be nil: traces=%v logs=%v", res.Traces, res.Logs)
	}
	if res.Metrics == nil {
		t.Errorf("present metrics signal should resolve")
	}
}

func TestResolveSamplerMapping(t *testing.T) {
	cases := []struct {
		name      string
		sampling  *ksquadv1.SamplingConfig
		wantNil   bool
		wantType  string
		wantRatio float64
	}{
		{"nil", nil, true, "", 0},
		{"always_on", &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeAlwaysOn}, false, "always_on", 0},
		{"always_off", &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeAlwaysOff}, false, "always_off", 0},
		{"probabilistic", &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeProbabilistic, Ratio: float64Ptr(0.3)}, false, "probabilistic", 0.3},
		{"probabilistic nil ratio", &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeProbabilistic}, false, "probabilistic", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &ksquadv1.OTelConfig{Spec: ksquadv1.OTelConfigSpec{
				Traces: &ksquadv1.SignalRouting{Endpoint: "c:4317", Protocol: ksquadv1.ExportProtocolGRPC, Sampling: tc.sampling},
			}}
			res := Resolve(context.Background(), cr, fakeSecrets{}, systemNS)
			s := res.Traces.Sampler
			if tc.wantNil {
				if s != nil {
					t.Fatalf("want nil sampler, got %+v", s)
				}
				return
			}
			if s == nil || s.Type != tc.wantType || s.Ratio != tc.wantRatio {
				t.Fatalf("sampler = %+v, want type=%q ratio=%v", s, tc.wantType, tc.wantRatio)
			}
		})
	}
}

func TestResolveAuthHeaderFromSecret(t *testing.T) {
	const token = "SENTINEL-TOKEN-DO-NOT-LOG"
	cr := &ksquadv1.OTelConfig{Spec: ksquadv1.OTelConfigSpec{
		Traces: &ksquadv1.SignalRouting{
			Endpoint: "c:4317", Protocol: ksquadv1.ExportProtocolGRPC,
			Auth: &ksquadv1.SecretKeyReference{Name: "otel-auth", Key: "token"},
		},
	}}
	secrets := fakeSecrets{systemNS + "/otel-auth/token": []byte(token)}
	res := Resolve(context.Background(), cr, secrets, systemNS)
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if got := res.Traces.Headers["Authorization"]; got != token {
		t.Errorf("Authorization header = %q, want %q", got, token)
	}
}

func TestResolveAuthDefaultsKeyAndNamespace(t *testing.T) {
	const token = "SENTINEL-TOKEN-DO-NOT-LOG"
	cr := &ksquadv1.OTelConfig{Spec: ksquadv1.OTelConfigSpec{
		Traces: &ksquadv1.SignalRouting{
			Endpoint: "c:4317", Protocol: ksquadv1.ExportProtocolGRPC,
			// Empty Key -> defaults to "token"; empty Namespace -> systemNS.
			Auth: &ksquadv1.SecretKeyReference{Name: "otel-auth"},
		},
	}}
	secrets := fakeSecrets{systemNS + "/otel-auth/token": []byte(token)}
	res := Resolve(context.Background(), cr, secrets, systemNS)
	if res.Traces == nil || res.Traces.Headers["Authorization"] != token {
		t.Fatalf("expected defaulted key/namespace to resolve token; got %+v (errs %v)", res.Traces, res.Errors)
	}
}

func TestResolveSecretErrorFallsBackToStdout(t *testing.T) {
	const token = "SENTINEL-TOKEN-DO-NOT-LOG"
	cr := &ksquadv1.OTelConfig{Spec: ksquadv1.OTelConfigSpec{
		Traces: &ksquadv1.SignalRouting{
			Endpoint: "c:4317", Protocol: ksquadv1.ExportProtocolGRPC,
			Auth: &ksquadv1.SecretKeyReference{Name: "missing-secret", Key: "token"},
		},
	}}
	// Empty store: the secret is absent. Include a sentinel elsewhere to be sure
	// no value leaks into the error path.
	secrets := fakeSecrets{systemNS + "/other/token": []byte(token)}
	res := Resolve(context.Background(), cr, secrets, systemNS)

	if res.Traces != nil {
		t.Fatalf("signal with unresolved secret must be nil (stdout), got %+v", res.Traces)
	}
	if len(res.Errors) != 1 || res.Errors[0].Signal != "traces" {
		t.Fatalf("want one traces SignalError, got %v", res.Errors)
	}
	msg := res.Errors[0].Err.Error()
	if !strings.Contains(msg, "missing-secret") {
		t.Errorf("error should name the Secret: %q", msg)
	}
	if strings.Contains(msg, token) {
		t.Errorf("error must NOT contain the token value: %q", msg)
	}
}

// TestResolveMissingKeyFallsBackAndNeverLogsValue covers the missing-KEY (vs
// missing-secret) branch and re-asserts the no-value-in-error guarantee across
// every SignalError produced.
func TestResolveMissingKeyFallsBackAndNeverLogsValue(t *testing.T) {
	const token = "SENTINEL-TOKEN-DO-NOT-LOG"
	cr := &ksquadv1.OTelConfig{Spec: ksquadv1.OTelConfigSpec{
		Metrics: &ksquadv1.SignalRouting{
			Endpoint: "c:4317", Protocol: ksquadv1.ExportProtocolGRPC,
			Auth: &ksquadv1.SecretKeyReference{Name: "otel-auth", Key: "wrong-key"},
		},
	}}
	// The secret exists but under a different key; the value is present in-store.
	secrets := fakeSecrets{systemNS + "/otel-auth/token": []byte(token)}
	res := Resolve(context.Background(), cr, secrets, systemNS)

	if res.Metrics != nil {
		t.Fatalf("missing key must fall back to stdout, got %+v", res.Metrics)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("want one SignalError, got %v", res.Errors)
	}
	for _, se := range res.Errors {
		if strings.Contains(se.Err.Error(), token) {
			t.Errorf("SignalError leaks token value: %q", se.Err.Error())
		}
	}
}

func TestPick(t *testing.T) {
	if got := Pick(nil); got != nil {
		t.Errorf("Pick(nil) = %v, want nil", got)
	}
	named := func(n string) ksquadv1.OTelConfig {
		return ksquadv1.OTelConfig{ObjectMeta: metav1.ObjectMeta{Name: n}}
	}
	// "default" wins regardless of order.
	got := Pick([]ksquadv1.OTelConfig{named("zeta"), named("default"), named("alpha")})
	if got == nil || got.Name != "default" {
		t.Errorf(`Pick prefers "default", got %v`, got)
	}
	// No "default" -> lexically-first.
	got = Pick([]ksquadv1.OTelConfig{named("zeta"), named("beta"), named("alpha")})
	if got == nil || got.Name != "alpha" {
		t.Errorf("Pick lexical fallback = %v, want alpha", got)
	}
}
