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

// This file renders the collector egress overlay for story 13.8 (ISI-3724),
// per the ratified mechanism ADR-0008 (M1 + config-ownership (b): layered
// multi-`--config` merge).
//
// The OTel Collector gateway (story 13.7) loads two `--config` sources that
// confmap deep-merges: maps merge, scalars and sequences are REPLACED by the
// last source that sets them. Helm owns the base config — receivers, every
// processor INCLUDING the redaction/transform pipeline, tail_sampling, and the
// safe stdout/debug default exporters — and it stays in GitOps, human-reviewed.
// The operator owns only this small overlay: the vendor `exporters:` block plus
// the per-signal `service.pipelines.*.exporters` override, resolved from the
// applied OTelConfig CR. Because sequences are replaced, a base pipeline's
// `exporters: [debug]` is cleanly overridden by the overlay's
// `exporters: [otlphttp/vendor_traces]` — redaction still runs upstream because
// it is a processor in the base pipeline the overlay never touches.
//
// This renderer is pure: it produces the overlay YAML string and reads no
// Secret and opens no connection. The controller that writes it into the
// `collector-egress` ConfigMap and rolls the collector (ADR-0008 §Mechanism
// steps 3–5) is the wiring step, gated on the DevOps Helm base/egress split
// (ISI-3747). Auth is never rendered as a value — only the env indirection
// `${env:KSQUAD_OTLP_AUTH}` the base Deployment already injects from the Secret.
package telemetry

import (
	"fmt"

	"sigs.k8s.io/yaml"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// vendorAuthEnvRef is the OTel confmap env-substitution reference emitted as the
// Authorization header value in the overlay. The collector Deployment injects
// the actual token into this env var from the auth Secret (ADR-0008); the token
// value itself is never written into any ConfigMap.
const vendorAuthEnvRef = "${env:KSQUAD_OTLP_AUTH}"

// baseSignalExporters are the base-config exporters an overlay must preserve for
// a signal when it adds the vendor exporter. Metrics keep the in-cluster
// `prometheus` scrape surface (the §9 SLO rules read it) alongside the vendor
// exporter; traces and logs replace the base `debug` default outright.
var baseSignalExporters = map[string][]string{
	"metrics": {"prometheus"},
}

// RenderEgressOverlay renders the collector egress overlay YAML from an applied
// OTelConfig. It returns the empty string (no overlay) when cfg is nil or
// configures no signal — the base config's stdout/debug default then holds and
// nothing is exported off-cluster, with redaction still enforced (opt-in,
// ISI-3103). Each configured signal contributes one vendor exporter and a
// pipeline override routing that signal to it (plus any preserved base
// exporter). Auth is emitted as the env reference, never a value.
func RenderEgressOverlay(cfg *ksquadv1alpha1.OTelConfig) (string, error) {
	if cfg == nil {
		return "", nil
	}

	// Deterministic signal order so the rendered YAML (and thus the ConfigMap
	// content, and thus the rollout trigger) is stable for an unchanged spec.
	signals := []struct {
		name    string
		routing *ksquadv1alpha1.SignalRouting
	}{
		{"traces", cfg.Spec.Traces},
		{"metrics", cfg.Spec.Metrics},
		{"logs", cfg.Spec.Logs},
	}

	exporters := map[string]any{}
	pipelines := map[string]any{}

	for _, s := range signals {
		if s.routing == nil {
			continue
		}
		auth := ""
		if s.routing.Auth != nil {
			auth = vendorAuthEnvRef
		}
		target, err := TargetFromRouting(s.routing, auth)
		if err != nil {
			return "", fmt.Errorf("telemetry: egress overlay for %s: %w", s.name, err)
		}
		name := vendorExporterName(target.Protocol, s.name)
		exporters[name] = vendorExporter(target)
		pipelines[s.name] = map[string]any{
			"exporters": append(append([]string{}, baseSignalExporters[s.name]...), name),
		}
	}

	if len(exporters) == 0 {
		return "", nil
	}

	overlay := map[string]any{
		"exporters": exporters,
		"service": map[string]any{
			"pipelines": pipelines,
		},
	}
	out, err := yaml.Marshal(overlay)
	if err != nil {
		return "", fmt.Errorf("telemetry: marshal egress overlay: %w", err)
	}
	return string(out), nil
}

// vendorExporterName is the collector exporter id for a signal: the OTLP
// exporter type (grpc→otlp, http/*→otlphttp) qualified with a per-signal name so
// signals routed to different backends never collide, and each pipeline
// references exactly its own exporter.
func vendorExporterName(proto ksquadv1alpha1.ExportProtocol, signal string) string {
	kind := "otlphttp"
	if proto == ksquadv1alpha1.ExportProtocolGRPC {
		kind = "otlp"
	}
	return kind + "/vendor_" + signal
}

// vendorExporter builds the collector exporter config map from a resolved
// ExportTarget. tls.insecure carries the resolved transport posture; headers is
// emitted only when the routing had an auth ref, and only ever as the env
// reference (never a token value).
func vendorExporter(t ExportTarget) map[string]any {
	exp := map[string]any{
		"endpoint": t.Endpoint,
		"tls": map[string]any{
			"insecure": t.Insecure,
		},
	}
	if len(t.Headers) > 0 {
		headers := make(map[string]any, len(t.Headers))
		for k, v := range t.Headers {
			headers[k] = v
		}
		exp["headers"] = headers
	}
	return exp
}
