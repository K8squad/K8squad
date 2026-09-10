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

package main

import (
	"testing"

	"github.com/K8squad/K8squad/pkg/telemetry"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestEnvOTLPSignalExport(t *testing.T) {
	t.Run("no endpoint yields nil (keep stdout)", func(t *testing.T) {
		if got := envOTLPSignalExport(envFrom(nil)); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})

	t.Run("blank endpoint yields nil", func(t *testing.T) {
		got := envOTLPSignalExport(envFrom(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "   "}))
		if got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})

	t.Run("endpoint defaults protocol to grpc", func(t *testing.T) {
		got := envOTLPSignalExport(envFrom(map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel-gateway-collector.observability:4317",
		}))
		if got == nil {
			t.Fatal("want non-nil export")
		}
		if got.Protocol != "grpc" {
			t.Errorf("protocol: want grpc, got %q", got.Protocol)
		}
		if got.Endpoint != "http://otel-gateway-collector.observability:4317" {
			t.Errorf("endpoint passed through unexpectedly: %q", got.Endpoint)
		}
	})

	t.Run("explicit protocol passes through", func(t *testing.T) {
		got := envOTLPSignalExport(envFrom(map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "https://oat05854.live.dynatrace.com/api/v2/otlp",
			"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		}))
		if got == nil {
			t.Fatal("want non-nil export")
		}
		if got.Protocol != "http/protobuf" {
			t.Errorf("protocol: want http/protobuf, got %q", got.Protocol)
		}
	})
}

func TestApplyEnvOTLPFallback(t *testing.T) {
	env := &telemetry.SignalExport{Protocol: "grpc", Endpoint: "http://gw:4317"}

	t.Run("nil env fills nothing (stdout preserved)", func(t *testing.T) {
		opts := &telemetry.Options{}
		if filled := applyEnvOTLPFallback(opts, nil); filled != nil {
			t.Fatalf("want no signals filled, got %v", filled)
		}
		if opts.Traces != nil || opts.Metrics != nil || opts.Logs != nil {
			t.Fatal("no signal should have been set")
		}
	})

	t.Run("all-nil signals get the env fallback", func(t *testing.T) {
		opts := &telemetry.Options{}
		filled := applyEnvOTLPFallback(opts, env)
		if len(filled) != 3 {
			t.Fatalf("want 3 signals filled, got %v", filled)
		}
		for _, se := range []*telemetry.SignalExport{opts.Traces, opts.Metrics, opts.Logs} {
			if se == nil || se.Endpoint != "http://gw:4317" || se.Protocol != "grpc" {
				t.Errorf("signal not set to env target: %+v", se)
			}
		}
		// Each signal must be a distinct struct, not an alias.
		if opts.Traces == opts.Metrics || opts.Metrics == opts.Logs || opts.Traces == opts.Logs {
			t.Error("signals must not alias the same struct")
		}
	})

	t.Run("CR-routed signal keeps precedence", func(t *testing.T) {
		crTraces := &telemetry.SignalExport{Protocol: "http/protobuf", Endpoint: "https://dt/api/v2/otlp/v1/traces"}
		opts := &telemetry.Options{Traces: crTraces}
		filled := applyEnvOTLPFallback(opts, env)
		if len(filled) != 2 {
			t.Fatalf("want metrics+logs filled, got %v", filled)
		}
		if opts.Traces != crTraces {
			t.Error("CR-routed traces must be untouched")
		}
		if opts.Metrics == nil || opts.Logs == nil {
			t.Error("metrics and logs should have been filled from env")
		}
	})
}
