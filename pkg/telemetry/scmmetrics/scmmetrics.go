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

// Package scmmetrics registers the GitHub-sync observability instruments on the
// process OTel meter (ISI-4395, child of ISI-4229 — the "a dropped webhook, a
// nil-Client panic, a provider error and a stale mirror all look identical from
// outside" gap).
//
// Before this package the scm sync path emitted logr lines only: the operator's
// reposync reconciler and the scm-webhook ingress created NO metric on
// telemetry.Meter(), so nothing about webhook acceptance, sync outcome, sync
// latency, reconcile panics, mirror freshness or provider rate-limit headroom
// ever reached the OTLP push path. The instruments registered here close that:
//
//	ksquad.scm.webhook.total{event,outcome}          counter    webhook deliveries by event + accept/reject/error
//	ksquad.scm.sync.total{provider,trigger,reason}   counter    reconcile passes by provider/trigger/reason taxonomy
//	ksquad.scm.sync.duration{provider,trigger}       histogram (s)  per-pass reconcile latency
//	ksquad.scm.sync.panics.total{provider}           counter    reconcile panics recovered + re-raised (GH-2)
//	ksquad.scm.mirror.age{project}                   gauge (s)  now - last successful mirror time
//	ksquad.scm.provider.rate_limit.remaining{provider} gauge    last-seen provider rate-limit headroom
//
// Cardinality firewall (NFR-OBS3): every label is BOUNDED. event is the fixed
// provider event vocabulary, outcome/reason/trigger are fixed enums, provider is
// the small provider set, and project is the tenant Project set (bounded by the
// fleet, one series per mirrored repo). repo URLs, run.id, work-item refs and
// delivery IDs are NEVER metric labels — those stay on spans and events.
//
// Register is process-agnostic: the scm-webhook binary records only
// RecordWebhook; the operator records the sync/panic/rate/age surface. An
// unused instrument simply exports nothing, so both processes call the same
// Register.
package scmmetrics

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Instrument names (OTel dot-delimited; the Prometheus bridge renders these as
// ksquad_scm_webhook_total, ksquad_scm_sync_duration_seconds, etc. — the names
// the ISI-4229 spec/dashboard use).
const (
	webhookTotalName  = "ksquad.scm.webhook.total"
	syncTotalName     = "ksquad.scm.sync.total"
	syncDurationName  = "ksquad.scm.sync.duration"
	syncPanicsName    = "ksquad.scm.sync.panics.total"
	mirrorAgeName     = "ksquad.scm.mirror.age"
	rateRemainingName = "ksquad.scm.provider.rate_limit.remaining"
)

// Bounded attribute keys.
const (
	attrEvent    = "event"
	attrOutcome  = "outcome"
	attrProvider = "provider"
	attrTrigger  = "trigger"
	attrReason   = "reason"
	attrProject  = "project"
)

// Webhook outcomes — the bounded value set for the outcome label.
const (
	OutcomeAccepted = "accepted" // good credential, reconcile triggered
	OutcomeRejected = "rejected" // dropped at the verify gate (uniform 401)
	OutcomeError    = "error"    // could not process (bad method, patch failed, overloaded)
)

// Sync triggers — the bounded value set for the trigger label.
const (
	TriggerWebhook = "webhook" // an external trigger annotation arrived since the last mirror (webhook or manual "Sync now")
	TriggerPoll    = "poll"    // the scheduled poll-interval requeue, or a spec change
)

// SyncOutcome is one recorded reconcile pass. Reason reuses the reposync reason
// taxonomy (Synced|ProviderError|MirrorWriteError|…) verbatim so the metric and
// the Project SyncReady condition speak the same vocabulary.
type SyncOutcome struct {
	Provider string
	Trigger  string
	Reason   string
	Project  string        // namespace/name; updates the mirror-age gauge iff Success
	Duration time.Duration // wall-clock of the pass
	Success  bool          // true iff the mirror upsert completed
}

// Metrics holds the registered scm instruments plus the cached gauge state.
// The zero value is not usable; construct with Register.
type Metrics struct {
	webhookTotal metric.Int64Counter
	syncTotal    metric.Int64Counter
	syncDuration metric.Float64Histogram
	syncPanics   metric.Int64Counter

	now func() time.Time

	mu            sync.RWMutex
	mirrorSuccess map[string]time.Time // project -> last successful mirror time
	rateRemaining map[string]int64     // provider -> last-seen rate-limit headroom
}

// Register creates the scm instruments on meter and wires the two observable
// gauges (mirror age, provider rate-limit headroom) to the cached state the
// recording methods maintain. It returns an error only if the meter rejects an
// instrument name, which never happens for these static names.
func Register(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{
		now:           time.Now,
		mirrorSuccess: map[string]time.Time{},
		rateRemaining: map[string]int64{},
	}

	var err error
	if m.webhookTotal, err = meter.Int64Counter(
		webhookTotalName,
		metric.WithDescription("SCM webhook deliveries by event and outcome (ISI-4395 GH-1)."),
	); err != nil {
		return nil, err
	}
	if m.syncTotal, err = meter.Int64Counter(
		syncTotalName,
		metric.WithDescription("SCM reconcile passes by provider, trigger and reason (ISI-4395 GH-3)."),
	); err != nil {
		return nil, err
	}
	if m.syncDuration, err = meter.Float64Histogram(
		syncDurationName,
		metric.WithUnit("s"),
		metric.WithDescription("SCM reconcile pass latency in seconds (ISI-4395 GH-3)."),
	); err != nil {
		return nil, err
	}
	if m.syncPanics, err = meter.Int64Counter(
		syncPanicsName,
		metric.WithDescription("SCM reconcile panics recovered and re-raised (ISI-4395 GH-2)."),
	); err != nil {
		return nil, err
	}

	if _, err = meter.Float64ObservableGauge(
		mirrorAgeName,
		metric.WithUnit("s"),
		metric.WithDescription("Seconds since each Project's last successful mirror pass (ISI-4395 GH-3)."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			m.mu.RLock()
			defer m.mu.RUnlock()
			now := m.now()
			for project, last := range m.mirrorSuccess {
				o.Observe(now.Sub(last).Seconds(), metric.WithAttributes(attribute.String(attrProject, project)))
			}
			return nil
		}),
	); err != nil {
		return nil, err
	}

	if _, err = meter.Int64ObservableGauge(
		rateRemainingName,
		metric.WithDescription("Last-seen provider rate-limit headroom (requests remaining) by provider (ISI-4395 GH-3)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			m.mu.RLock()
			defer m.mu.RUnlock()
			for provider, remaining := range m.rateRemaining {
				o.Observe(remaining, metric.WithAttributes(attribute.String(attrProvider, provider)))
			}
			return nil
		}),
	); err != nil {
		return nil, err
	}

	return m, nil
}

// RecordWebhook counts one webhook delivery. A nil *Metrics is a no-op so the
// ingress can call it unconditionally (telemetry.Setup may have been skipped in
// a stripped test binary).
func (m *Metrics) RecordWebhook(ctx context.Context, event, outcome string) {
	if m == nil {
		return
	}
	if event == "" {
		event = "unknown"
	}
	m.webhookTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attrEvent, event),
		attribute.String(attrOutcome, outcome),
	))
}

// RecordSync counts one reconcile pass and records its latency. On Success it
// stamps the mirror-age gauge for the Project so a stalled reconcile shows an
// ever-growing age rather than silently dropping off the series.
func (m *Metrics) RecordSync(ctx context.Context, out SyncOutcome) {
	if m == nil {
		return
	}
	provider := out.Provider
	if provider == "" {
		provider = "unknown"
	}
	m.syncTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attrProvider, provider),
		attribute.String(attrTrigger, out.Trigger),
		attribute.String(attrReason, out.Reason),
	))
	m.syncDuration.Record(ctx, out.Duration.Seconds(), metric.WithAttributes(
		attribute.String(attrProvider, provider),
		attribute.String(attrTrigger, out.Trigger),
	))
	if out.Success && out.Project != "" {
		m.mu.Lock()
		m.mirrorSuccess[out.Project] = m.now()
		m.mu.Unlock()
	}
}

// RecordPanic counts one recovered-and-re-raised reconcile panic (GH-2). The
// caller re-panics after recording so controller-runtime still crashes the
// worker — the counter makes the crash countable, it does not swallow it.
func (m *Metrics) RecordPanic(ctx context.Context, provider string) {
	if m == nil {
		return
	}
	if provider == "" {
		provider = "unknown"
	}
	m.syncPanics.Add(ctx, 1, metric.WithAttributes(attribute.String(attrProvider, provider)))
}

// ObserveRateLimit records the last-seen provider rate-limit headroom for the
// rate_limit_remaining gauge. A negative remaining is ignored (unknown).
func (m *Metrics) ObserveRateLimit(provider string, remaining int64) {
	if m == nil || provider == "" || remaining < 0 {
		return
	}
	m.mu.Lock()
	m.rateRemaining[provider] = remaining
	m.mu.Unlock()
}

// ForgetProject drops a Project's cached mirror-age series — call on Project
// deletion so a removed repo stops reporting a growing age forever.
func (m *Metrics) ForgetProject(project string) {
	if m == nil || project == "" {
		return
	}
	m.mu.Lock()
	delete(m.mirrorSuccess, project)
	m.mu.Unlock()
}
