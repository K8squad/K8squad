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

// envexport.go holds the OTEL_EXPORTER_OTLP_* env fallback shared by every
// k8squad binary that runs the telemetry spine outside the operator's
// OTelConfig CR resolution — today: the in-pod shim (run + supervisor). The
// operator keeps its own copy in cmd/operator (ISI-4102) because it layers
// the env UNDER the CR; these helpers exist so the shim does not duplicate
// the parsing rules a third time.
//
// Semantics (identical to the operator's, M1.2 telemetry leg): the Helm chart
// injects OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_PROTOCOL pointing
// at the observability gateway on every workload, and the operator stamps the
// same pair onto sandbox pods via WithPodEnv. A binary that ignores the env
// keeps every signal on the stdout default and its spans never leave the pod.
package telemetry

import "strings"

// EnvSignalExport builds a *SignalExport from the standard
// OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_PROTOCOL env pair. It
// returns nil when no endpoint is set, so a deployment that leaves the env
// unset keeps the stdout default. Protocol defaults to "grpc" (the
// observability gateway's OTLP port) when unset. The endpoint is passed
// through verbatim: parseGRPCEndpoint strips an http:// scheme and dials the
// in-cluster gateway insecurely.
func EnvSignalExport(getenv func(string) string) *SignalExport {
	endpoint := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		return nil
	}
	protocol := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	if protocol == "" {
		protocol = "grpc"
	}
	return &SignalExport{Protocol: protocol, Endpoint: endpoint}
}

// ApplyEnvOTLPFallback fills each signal still on the stdout default (nil)
// with the env-derived OTLP target, if one is configured. A signal already
// routed (e.g. by the OTelConfig CR in the operator) is untouched, so
// declarative routing keeps precedence. Each filled signal gets its own copy
// so the three never alias. It returns the names of the signals it filled,
// for logging.
func ApplyEnvOTLPFallback(opts *Options, env *SignalExport) []string {
	if env == nil {
		return nil
	}
	var filled []string
	if opts.Traces == nil {
		se := *env
		opts.Traces = &se
		filled = append(filled, "traces")
	}
	if opts.Metrics == nil {
		se := *env
		opts.Metrics = &se
		filled = append(filled, "metrics")
	}
	if opts.Logs == nil {
		se := *env
		opts.Logs = &se
		filled = append(filled, "logs")
	}
	return filled
}
