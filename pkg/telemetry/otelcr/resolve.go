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

// Package otelcr is the ONLY layer that translates the cluster-scoped OTelConfig
// CR (api/v1alpha1) plus the Secrets it references into the neutral
// telemetry.SignalExport shapes the spine consumes (ISI-3620, Story C). It
// imports api/v1alpha1 and reads Secrets through a narrow SecretGetter seam, so
// it is fully unit-testable without a live cluster and keeps pkg/telemetry
// kube-free.
//
// Fail-open-to-stdout is the contract (C-AC2/C-AC3): an absent CR, an absent
// signal, or ANY Secret error leaves that signal's *telemetry.SignalExport nil,
// which the spine reads as "keep the stdout default". Resolve never logs and a
// SignalError never carries a token VALUE — only the Secret name may appear.
package otelcr

import (
	"context"
	"fmt"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/telemetry"
)

// SecretGetter fetches one key out of one Secret. Its only production
// implementation reads a corev1.Secret through the kube client (see
// cmd/operator); tests pass a fake. It returns an error when the Secret or the
// key is absent — that error must NOT contain the secret value.
type SecretGetter interface {
	Get(ctx context.Context, namespace, name, key string) ([]byte, error)
}

// SignalError records why a signal fell back to the stdout default. Its Err may
// name the Secret but MUST NOT contain the resolved value. It feeds Story D's
// per-signal status; Resolve itself does no logging.
type SignalError struct {
	Signal string
	Err    error
}

// Resolved carries the per-signal exports plus any per-signal fallback errors. A
// nil *telemetry.SignalExport means the spine keeps stdout for that signal.
type Resolved struct {
	Traces  *telemetry.SignalExport
	Metrics *telemetry.SignalExport
	Logs    *telemetry.SignalExport
	Errors  []SignalError
}

// Resolve projects a single OTelConfig CR onto the neutral per-signal exports.
// Each present signal (Traces/Metrics/Logs) is resolved independently: a Secret
// error on one signal drops ONLY that signal to stdout and appends a
// SignalError; the others are unaffected. A nil cr yields an empty Resolved
// (all stdout).
func Resolve(ctx context.Context, cr *ksquadv1.OTelConfig, secrets SecretGetter, systemNamespace string) Resolved {
	var res Resolved
	if cr == nil {
		return res
	}
	res.Traces = resolveSignal(ctx, "traces", cr.Spec.Traces, secrets, systemNamespace, &res)
	res.Metrics = resolveSignal(ctx, "metrics", cr.Spec.Metrics, secrets, systemNamespace, &res)
	res.Logs = resolveSignal(ctx, "logs", cr.Spec.Logs, secrets, systemNamespace, &res)
	return res
}

// resolveSignal maps one CRD SignalRouting onto a telemetry.SignalExport,
// resolving auth through secrets. A nil routing returns nil (signal not
// exported). A Secret error returns nil AND records a SignalError on res, so the
// signal silently keeps stdout (C-AC3).
func resolveSignal(ctx context.Context, name string, r *ksquadv1.SignalRouting, secrets SecretGetter, systemNamespace string, res *Resolved) *telemetry.SignalExport {
	if r == nil {
		return nil
	}
	se := &telemetry.SignalExport{
		// Protocol passes through verbatim: the CRD enum values ("grpc",
		// "http/protobuf", "http/json") ARE the neutral spine's protocol strings.
		Protocol: string(r.Protocol),
		Endpoint: r.Endpoint,
		Sampler:  samplerSpecFor(r.Sampling),
	}
	// ponytail: v1 corner — per-signal CRD resourceAttributes are not merged into
	// the operator's shared OTel resource yet; deferred follow-up.

	if r.Auth != nil {
		ns := r.Auth.Namespace
		if ns == "" {
			ns = systemNamespace
		}
		key := r.Auth.Key
		if key == "" {
			key = "token"
		}
		val, err := secrets.Get(ctx, ns, r.Auth.Name, key)
		if err != nil {
			// Fail open to stdout for THIS signal only; never surface the value.
			res.Errors = append(res.Errors, SignalError{
				Signal: name,
				Err:    fmt.Errorf("resolve auth secret %s/%s: %w", ns, r.Auth.Name, err),
			})
			return nil
		}
		se.Headers = map[string]string{"Authorization": string(val)}
	}
	return se
}

// samplerSpecFor maps the CRD SamplingConfig (traces only) onto the neutral
// SamplerSpec. nil -> nil (spine keeps its default sampler).
func samplerSpecFor(s *ksquadv1.SamplingConfig) *telemetry.SamplerSpec {
	if s == nil {
		return nil
	}
	switch s.Type {
	case ksquadv1.SamplingTypeAlwaysOn:
		return &telemetry.SamplerSpec{Type: "always_on"}
	case ksquadv1.SamplingTypeAlwaysOff:
		return &telemetry.SamplerSpec{Type: "always_off"}
	case ksquadv1.SamplingTypeProbabilistic:
		var ratio float64
		if s.Ratio != nil {
			ratio = *s.Ratio
		}
		return &telemetry.SamplerSpec{Type: "probabilistic", Ratio: ratio}
	default:
		return nil
	}
}

// Pick chooses one OTelConfig deterministically, mirroring pickOTelConfig in
// internal/apiserver/otelconfig.go: prefer the CR named "default", else the
// lexically-first by name. Returns nil when items is empty.
func Pick(items []ksquadv1.OTelConfig) *ksquadv1.OTelConfig {
	if len(items) == 0 {
		return nil
	}
	best := &items[0]
	for i := range items {
		c := &items[i]
		if c.Name == "default" {
			return c
		}
		if c.Name < best.Name {
			best = c
		}
	}
	return best
}
