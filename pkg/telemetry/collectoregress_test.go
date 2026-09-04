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
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// parseOverlay unmarshals rendered overlay YAML for structural assertions.
func parseOverlay(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("overlay is not valid YAML: %v\n%s", err, s)
	}
	return m
}

func TestRenderEgressOverlayNilOrEmpty(t *testing.T) {
	if got, err := RenderEgressOverlay(nil); err != nil || got != "" {
		t.Fatalf("nil cfg: got %q err %v, want empty", got, err)
	}
	// A cfg with no signals renders no overlay so the base stdout/debug default
	// holds (opt-in: nothing exported off-cluster).
	empty := &ksquadv1alpha1.OTelConfig{}
	if got, err := RenderEgressOverlay(empty); err != nil || got != "" {
		t.Fatalf("empty cfg: got %q err %v, want empty", got, err)
	}
}

func TestRenderEgressOverlayTracesHTTP(t *testing.T) {
	cfg := &ksquadv1alpha1.OTelConfig{
		Spec: ksquadv1alpha1.OTelConfigSpec{
			Traces: &ksquadv1alpha1.SignalRouting{
				Endpoint: "https://otlp.dynatrace.com/api/v2/otlp/v1/traces",
				Protocol: ksquadv1alpha1.ExportProtocolHTTPProtobuf,
				Auth:     &ksquadv1alpha1.SecretKeyReference{Name: "otlp-token", Key: "token"},
			},
		},
	}
	out, err := RenderEgressOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := parseOverlay(t, out)

	exporters := m["exporters"].(map[string]any)
	exp, ok := exporters["otlphttp/vendor_traces"].(map[string]any)
	if !ok {
		t.Fatalf("missing otlphttp/vendor_traces exporter in %v", exporters)
	}
	if exp["endpoint"] != "https://otlp.dynatrace.com/api/v2/otlp/v1/traces" {
		t.Errorf("endpoint = %v", exp["endpoint"])
	}
	// auth must be the env reference, never a token value.
	headers := exp["headers"].(map[string]any)
	if headers["Authorization"] != vendorAuthEnvRef {
		t.Errorf("Authorization = %v, want env ref", headers["Authorization"])
	}
	if strings.Contains(out, "token") && !strings.Contains(out, "${env:") {
		t.Errorf("overlay may leak a secret value:\n%s", out)
	}

	pipelines := m["service"].(map[string]any)["pipelines"].(map[string]any)
	tp := pipelines["traces"].(map[string]any)["exporters"].([]any)
	if len(tp) != 1 || tp[0] != "otlphttp/vendor_traces" {
		t.Errorf("traces pipeline exporters = %v, want [otlphttp/vendor_traces]", tp)
	}
}

func TestRenderEgressOverlayGRPCInsecure(t *testing.T) {
	cfg := &ksquadv1alpha1.OTelConfig{
		Spec: ksquadv1alpha1.OTelConfigSpec{
			Traces: &ksquadv1alpha1.SignalRouting{
				Endpoint: "collector.obs.svc:4317",
				Protocol: ksquadv1alpha1.ExportProtocolGRPC,
			},
		},
	}
	out, err := RenderEgressOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := parseOverlay(t, out)
	exp := m["exporters"].(map[string]any)["otlp/vendor_traces"].(map[string]any)
	if exp["endpoint"] != "collector.obs.svc:4317" {
		t.Errorf("endpoint = %v", exp["endpoint"])
	}
	if tls := exp["tls"].(map[string]any); tls["insecure"] != true {
		t.Errorf("tls.insecure = %v, want true (bare grpc authority)", tls["insecure"])
	}
	// No auth ref ⇒ no headers block.
	if _, has := exp["headers"]; has {
		t.Errorf("unexpected headers block for auth-less routing")
	}
}

func TestRenderEgressOverlayMetricsPreservesPrometheus(t *testing.T) {
	cfg := &ksquadv1alpha1.OTelConfig{
		Spec: ksquadv1alpha1.OTelConfigSpec{
			Metrics: &ksquadv1alpha1.SignalRouting{
				Endpoint: "https://otlp.example.com/v1/metrics",
				Protocol: ksquadv1alpha1.ExportProtocolHTTPProtobuf,
			},
		},
	}
	out, err := RenderEgressOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := parseOverlay(t, out)
	mp := m["service"].(map[string]any)["pipelines"].(map[string]any)["metrics"].(map[string]any)["exporters"].([]any)
	if len(mp) != 2 || mp[0] != "prometheus" || mp[1] != "otlphttp/vendor_metrics" {
		t.Errorf("metrics pipeline exporters = %v, want [prometheus, otlphttp/vendor_metrics]", mp)
	}
}

func TestRenderEgressOverlayAllThreeSignals(t *testing.T) {
	cfg := &ksquadv1alpha1.OTelConfig{
		Spec: ksquadv1alpha1.OTelConfigSpec{
			Traces:  &ksquadv1alpha1.SignalRouting{Endpoint: "https://t.example.com/v1/traces", Protocol: ksquadv1alpha1.ExportProtocolHTTPProtobuf},
			Metrics: &ksquadv1alpha1.SignalRouting{Endpoint: "m.example.com:4317", Protocol: ksquadv1alpha1.ExportProtocolGRPC},
			Logs:    &ksquadv1alpha1.SignalRouting{Endpoint: "https://l.example.com/v1/logs", Protocol: ksquadv1alpha1.ExportProtocolHTTPJSON},
		},
	}
	out, err := RenderEgressOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := parseOverlay(t, out)
	exporters := m["exporters"].(map[string]any)
	for _, want := range []string{"otlphttp/vendor_traces", "otlp/vendor_metrics", "otlphttp/vendor_logs"} {
		if _, ok := exporters[want]; !ok {
			t.Errorf("missing exporter %q in %v", want, exporters)
		}
	}
	pipelines := m["service"].(map[string]any)["pipelines"].(map[string]any)
	for _, sig := range []string{"traces", "metrics", "logs"} {
		if _, ok := pipelines[sig]; !ok {
			t.Errorf("missing pipeline override for %q", sig)
		}
	}
}

func TestRenderEgressOverlayDeterministic(t *testing.T) {
	cfg := &ksquadv1alpha1.OTelConfig{
		Spec: ksquadv1alpha1.OTelConfigSpec{
			Traces:  &ksquadv1alpha1.SignalRouting{Endpoint: "https://t.example.com/v1/traces", Protocol: ksquadv1alpha1.ExportProtocolHTTPProtobuf},
			Metrics: &ksquadv1alpha1.SignalRouting{Endpoint: "m.example.com:4317", Protocol: ksquadv1alpha1.ExportProtocolGRPC},
		},
	}
	first, err := RenderEgressOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := RenderEgressOverlay(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != first {
			t.Fatalf("render not deterministic:\n--- first ---\n%s\n--- got ---\n%s", first, got)
		}
	}
}

func TestRenderEgressOverlayInvalidRoutingErrors(t *testing.T) {
	cfg := &ksquadv1alpha1.OTelConfig{
		Spec: ksquadv1alpha1.OTelConfigSpec{
			Traces: &ksquadv1alpha1.SignalRouting{
				Endpoint: "otlp.example.com/v1/traces", // http/* without scheme → invalid
				Protocol: ksquadv1alpha1.ExportProtocolHTTPProtobuf,
			},
		},
	}
	if _, err := RenderEgressOverlay(cfg); err == nil {
		t.Fatal("expected error for invalid routing")
	}
}
