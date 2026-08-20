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

package modelendpoint

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// The 5.11 fallback half of the 13.9 metric set (the ratelimit.* pair
// lands with ISI-2296/2297). Cardinality follows the 13.6 budget: the
// bounded label set is exactly {project, agent, role, primary_model,
// fallback_model} for activations and {project, agent, role} for duration;
// run.id and principal.id stay EXEMPLARS, never labels.
//
//	ksquad_fallback_activations_total     counter   5.11 switches fired
//	ksquad_fallback_duration_seconds      histogram time served on fallback
//
// Duration is observed when a fallback portion ENDS (segment closed) — the
// reconciler calls ObserveFallbackDuration with the portion length — so a
// crash between switch and close can under-observe but never
// double-observe the same portion.
var (
	metricsRegisterOnce sync.Once

	fallbackActivations = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ksquad_fallback_activations_total",
			Help: "Mid-Run fallback model switches fired on rate_limited signals (story 5.11, metric set 13.9).",
		},
		[]string{"project", "agent", "role", "primary_model", "fallback_model"},
	)

	fallbackDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ksquad_fallback_duration_seconds",
			Help:    "Seconds a Run served from its fallback model per portion (story 5.11, metric set 13.9).",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s .. ~34m
		},
		[]string{"project", "agent", "role"},
	)
)

// Metrics is the fallback metering seam (thin, swappable façade in the
// events.Metrics style so tests assert on emissions with a fake and the
// signal set is named in one place).
type Metrics interface {
	// IncFallbackActivation meters one 5.11 switch. dims carry the bounded
	// 13.9 label set.
	IncFallbackActivation(dims ActivationDims)
	// ObserveFallbackDuration meters one completed fallback portion.
	ObserveFallbackDuration(dims DurationDims, seconds float64)
	// Signals returns every signal this Metrics emits — the completeness
	// probe asserts the 5.11 pair is present.
	Signals() []string
}

// ActivationDims is the bounded label set of the activations counter.
type ActivationDims struct {
	Project       string
	Agent         string
	Role          string
	PrimaryModel  string
	FallbackModel string
}

// DurationDims is the bounded label set of the duration histogram (the
// model pair is intentionally absent — 13.9 pins duration to
// project/agent/role only, keeping the series count down).
type DurationDims struct {
	Project string
	Agent   string
	Role    string
}

// PrometheusMetrics implements Metrics on the two instruments above.
type PrometheusMetrics struct{}

// NewPrometheusMetrics registers the 5.11 instruments on the default
// registry (idempotent across calls) and returns the emitter.
func NewPrometheusMetrics() *PrometheusMetrics {
	metricsRegisterOnce.Do(func() {
		prometheus.MustRegister(fallbackActivations, fallbackDuration)
	})
	return &PrometheusMetrics{}
}

// IncFallbackActivation implements Metrics.
func (PrometheusMetrics) IncFallbackActivation(d ActivationDims) {
	fallbackActivations.WithLabelValues(d.Project, d.Agent, d.Role, d.PrimaryModel, d.FallbackModel).Inc()
}

// ObserveFallbackDuration implements Metrics.
func (PrometheusMetrics) ObserveFallbackDuration(d DurationDims, seconds float64) {
	fallbackDuration.WithLabelValues(d.Project, d.Agent, d.Role).Observe(seconds)
}

// Signals implements Metrics.
func (PrometheusMetrics) Signals() []string {
	return []string{
		"ksquad_fallback_activations_total",
		"ksquad_fallback_duration_seconds",
	}
}
