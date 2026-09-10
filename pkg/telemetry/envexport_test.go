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
	"reflect"
	"testing"
)

func TestEnvSignalExport_NoEndpoint(t *testing.T) {
	if got := EnvSignalExport(func(string) string { return "" }); got != nil {
		t.Fatalf("expected nil without OTEL_EXPORTER_OTLP_ENDPOINT, got %+v", got)
	}
}

func TestEnvSignalExport_DefaultsProtocolGRPC(t *testing.T) {
	env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": " http://otel-gateway-collector.observability:4317 "}
	got := EnvSignalExport(func(k string) string { return env[k] })
	if got == nil {
		t.Fatal("expected a SignalExport when the endpoint env is set")
	}
	if got.Protocol != "grpc" {
		t.Fatalf("protocol: want grpc default, got %q", got.Protocol)
	}
	if got.Endpoint != "http://otel-gateway-collector.observability:4317" {
		t.Fatalf("endpoint: want trimmed passthrough, got %q", got.Endpoint)
	}
}

func TestEnvSignalExport_ExplicitProtocol(t *testing.T) {
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://oat05854.dev.dynatracelabs.com/api/v2/otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
	}
	got := EnvSignalExport(func(k string) string { return env[k] })
	if got == nil || got.Protocol != "http/protobuf" {
		t.Fatalf("protocol: want http/protobuf passthrough, got %+v", got)
	}
}

func TestApplyEnvOTLPFallback_NilEnv(t *testing.T) {
	opts := &Options{}
	if filled := ApplyEnvOTLPFallback(opts, nil); filled != nil {
		t.Fatalf("nil env must fill nothing, got %v", filled)
	}
	if opts.Traces != nil || opts.Metrics != nil || opts.Logs != nil {
		t.Fatalf("nil env must leave every signal on stdout default, got %+v", opts)
	}
}

func TestApplyEnvOTLPFallback_FillsStdoutSignalsOnly(t *testing.T) {
	crRouted := &SignalExport{Protocol: "http/protobuf", Endpoint: "https://dt.example/otlp"}
	opts := &Options{Traces: crRouted}
	env := &SignalExport{Protocol: "grpc", Endpoint: "http://otel-gateway-collector.observability:4317"}

	filled := ApplyEnvOTLPFallback(opts, env)
	if !reflect.DeepEqual(filled, []string{"metrics", "logs"}) {
		t.Fatalf("filled: want [metrics logs], got %v", filled)
	}
	if opts.Traces != crRouted {
		t.Fatalf("CR-routed traces signal must keep precedence, got %+v", opts.Traces)
	}
	if opts.Metrics == nil || opts.Logs == nil {
		t.Fatalf("metrics/logs must be filled from env, got %+v", opts)
	}
	if opts.Metrics == opts.Logs {
		t.Fatal("filled signals must not alias the same *SignalExport")
	}
	if opts.Metrics.Protocol != env.Protocol || opts.Metrics.Endpoint != env.Endpoint {
		t.Fatalf("metrics: want a copy of the env export, got %+v", opts.Metrics)
	}
}
