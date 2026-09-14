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

// GitHub-sync (SCM) telemetry conventions — ISI-4395 GH-5 / WS-GH.
//
// GH-1..4 (PR #445) closed the end-to-end observability gap on the GitHub sync
// path so a dropped webhook, a nil-Client panic, a provider error and a stale
// mirror stop looking identical from outside. This file is the canonical,
// machine-readable record of the spans and metrics that path now emits, so they
// join the same single-source-of-truth registry as the run trace (registry.go)
// and can never silently drift: scm_conformance_test.go drives the REAL
// pkg/telemetry/scmmetrics emitter and asserts every Stable metric here is
// actually produced with the documented label set, and Render() folds these
// conventions into docs/observability/semantic-conventions.md.
//
// SPANS. The path emits four span shapes across three processes:
//
//   - scm.webhook.receive (scm-webhook ingress, GH-1) — one inbound delivery.
//   - scm.sync            (operator reposync Reconcile, GH-2) — one mirror pass;
//     joins the webhook trace via the injected traceparent annotation.
//   - scm.fetch.<kind>    (pkg/scm per-kind fetchers, GH-2) — a child of
//     scm.sync per entity kind (pull_requests/issues/check_runs/…).
//   - scm.sync.trigger    (apiserver manual "Sync now", GH-4) — proves a refresh
//     was a manual kick, distinct from the poll/webhook path.
//
// The apiserver GET .../github SERVER span is enriched in place (GH-4) rather
// than opened here; its ksquad.scm.* attributes are documented under
// SpanSCMStatusRead below.
//
// NAMESPACE NOTE. The operator/apiserver spans use the ksquad.scm.* namespace;
// the scm-webhook ingress span predates that convention and uses a bare scm.*
// namespace (scm.webhook.event/outcome, scm.project/namespace/provider). Both
// are documented here as emitted — a future cleanup can align the ingress span
// onto ksquad.scm.* (tracked as a WS-GH follow-up), and this registry is what
// will catch it if the keys change.
package ksqsemconv

// SCM span-name constants. scm.fetch.<kind> is parameterised by entity kind, so
// the registry documents the stable prefix; SpanSCMFetchPrefix is the join key.
const (
	SpanSCMWebhookReceive = "scm.webhook.receive"
	SpanSCMSync           = "scm.sync"
	SpanSCMFetchPrefix    = "scm.fetch."
	SpanSCMSyncTrigger    = "scm.sync.trigger"
	// SpanSCMStatusRead is the logical name for the apiserver GET .../github
	// server span the GH-4 enrichment decorates in place (the physical span
	// name is the HTTP route span; this entry documents its ksquad.scm.* attrs).
	SpanSCMStatusRead = "scm.status.read"
)

// SCMSpanConventions documents the GitHub-sync span family. These spans are
// emitted by the scm-webhook / operator / apiserver code today (Stability
// Stable), not by the run-trace toolusage.Mapper — so they live here and are
// guarded by scm_conformance_test.go, NOT by the run-trace conformance suite in
// conformance_test.go (which drives the mapper).
var SCMSpanConventions = []SpanConvention{
	{
		Name:  SpanSCMWebhookReceive,
		Brief: "One inbound SCM webhook delivery at the scm-webhook ingress (GH-1). Opens the sync trace; a W3C traceparent is injected into the trigger-annotation patch so the operator's scm.sync joins it. Webhook payload is never persisted and never travels on span attributes (PII posture).",
		Attributes: []Attribute{
			{Key: "scm.webhook.event", Type: TypeString, Requirement: Required, Stability: Stable, Workstream: "WS-GH",
				Brief: "The webhook event type (e.g. push, pull_request); \"unknown\" when absent."},
			{Key: "scm.webhook.outcome", Type: TypeString, Requirement: Required, Stability: Stable, Workstream: "WS-GH",
				Brief: "Delivery outcome (accepted|rejected|error). rejected is the uniform-401 verify gate; error is a processing failure."},
			{Key: "scm.project", Type: TypeString, Requirement: Conditional, Stability: Stable, Workstream: "WS-GH",
				Brief: "The Project the delivery maps to, when resolved (bare scm.* namespace — see NAMESPACE NOTE)."},
			{Key: "scm.namespace", Type: TypeString, Requirement: Conditional, Stability: Stable, Workstream: "WS-GH",
				Brief: "The Project's namespace, when resolved."},
			{Key: "scm.provider", Type: TypeString, Requirement: Conditional, Stability: Stable, Workstream: "WS-GH",
				Brief: "The SCM provider the trigger is forwarded to, when resolved."},
		},
	},
	{
		Name:  SpanSCMSync,
		Brief: "One operator reposync Reconcile / mirror pass (GH-2). Joins the inbound webhook trace via the traceparent annotation. A deferred recover records the error and increments ksquad.scm.sync.panics.total before re-panicking, so a nil Client/Providers/Store deref is visible rather than silent.",
		Attributes: []Attribute{
			{Key: "ksquad.scm.provider", Type: TypeString, Requirement: Required, Stability: Stable, Workstream: "WS-GH",
				Brief: "The SCM provider driving the pass (e.g. github)."},
			{Key: "ksquad.scm.trigger", Type: TypeString, Requirement: Required, Stability: Stable, Workstream: "WS-GH",
				Brief: "What kicked the pass (webhook|poll) — proves whether a refresh was event-driven or the scheduled requeue."},
			{Key: "ksquad.scm.reason", Type: TypeString, Requirement: Conditional, Stability: Stable, Workstream: "WS-GH",
				Brief: "The reconcile reason/condition (Synced|ProviderError|MirrorWriteError|CredentialMissing|…); the SyncReady condition vocabulary. err.Error() on the CredentialMissing path never embeds the BYO token."},
			{Key: "ksquad.scm.mirror.record_count", Type: TypeInt, Requirement: Conditional, Stability: Stable, Workstream: "WS-GH",
				Brief: "Total mirror records upserted this pass, when the pass reached the write phase."},
		},
	},
	{
		Name:  SpanSCMFetchPrefix + "<kind>",
		Brief: "A child of scm.sync per entity kind (GH-2): scm.fetch.pull_requests, scm.fetch.issues, scm.fetch.check_runs, scm.fetch.artifacts, scm.fetch.releases, scm.fetch.branches. Isolates which provider fetch is slow or failing.",
		Attributes: []Attribute{
			{Key: "ksquad.scm.record_count", Type: TypeInt, Requirement: Required, Stability: Stable, Workstream: "WS-GH",
				Brief: "Number of records the per-kind fetcher returned."},
		},
	},
	{
		Name:  SpanSCMSyncTrigger,
		Brief: "The apiserver manual \"Sync now\" path (GH-4). Carries trigger=manual so a dashboard can PROVE a mirror refresh was an operator kick, not the automatic webhook/poll pipeline.",
		Attributes: []Attribute{
			{Key: "ksquad.scm.trigger", Type: TypeString, Requirement: Required, Stability: Stable, Workstream: "WS-GH",
				Brief: "Always \"manual\" for this span."},
			{Key: "ksquad.scm.project", Type: TypeString, Requirement: Required, Stability: Stable, Workstream: "WS-GH",
				Brief: "namespace/name of the Project whose mirror was kicked."},
		},
	},
	{
		Name:  SpanSCMStatusRead,
		Brief: "The apiserver GET /api/projects/{id}/github server span, enriched in place (GH-4) with domain attrs + the freshness SLI so the GitHub-status tab read carries a mirror-age signal rather than being a blind spot.",
		Attributes: []Attribute{
			{Key: "ksquad.scm.project", Type: TypeString, Requirement: Required, Stability: Stable, Workstream: "WS-GH",
				Brief: "namespace/name of the Project being read."},
			{Key: "ksquad.scm.repo_url", Type: TypeString, Requirement: Recommended, Stability: Stable, Workstream: "WS-GH",
				Brief: "The mirrored repository URL (never the credential)."},
			{Key: "ksquad.scm.mirror.age_seconds", Type: TypeDouble, Requirement: Conditional, Stability: Stable, Workstream: "WS-GH",
				Brief: "Freshness SLI: now − Project.status.sync.lastSuccess, present when a successful mirror exists."},
			{Key: "ksquad.scm.result.pull_requests", Type: TypeInt, Requirement: Recommended, Stability: Stable, Workstream: "WS-GH",
				Brief: "Pull-request records served from the mirror."},
			{Key: "ksquad.scm.result.issues", Type: TypeInt, Requirement: Recommended, Stability: Stable, Workstream: "WS-GH",
				Brief: "Issue records served from the mirror."},
			{Key: "ksquad.scm.result.check_runs", Type: TypeInt, Requirement: Recommended, Stability: Stable, Workstream: "WS-GH",
				Brief: "Check-run records served from the mirror."},
			{Key: "ksquad.scm.result.artifacts", Type: TypeInt, Requirement: Recommended, Stability: Stable, Workstream: "WS-GH",
				Brief: "Artifact records served from the mirror."},
			{Key: "ksquad.scm.result.releases", Type: TypeInt, Requirement: Recommended, Stability: Stable, Workstream: "WS-GH",
				Brief: "Release records served from the mirror."},
			{Key: "ksquad.scm.result.branches", Type: TypeInt, Requirement: Recommended, Stability: Stable, Workstream: "WS-GH",
				Brief: "Branch records served from the mirror."},
		},
	},
}

// Instrument is the OTel metric instrument kind.
type Instrument string

const (
	InstrumentCounter         Instrument = "counter"
	InstrumentHistogram       Instrument = "histogram"
	InstrumentObservableGauge Instrument = "observable_gauge"
)

// MetricConvention is one entry in the metric registry: the canonical OTel
// (dotted) instrument name, its kind/unit, its label set and stability. The
// Prometheus rendering replaces dots with underscores (ksquad.scm.webhook.total
// → ksquad_scm_webhook_total).
type MetricConvention struct {
	// Name is the OTel instrument name as registered on the meter (dotted).
	Name string
	// Instrument is the instrument kind.
	Instrument Instrument
	// Unit is the UCUM unit ("s", "1", or "" when unitless).
	Unit string
	// Labels are the attribute keys the metric is dimensioned by. NEVER include
	// repo or run.id here — those are high-cardinality and belong on spans only.
	Labels     []string
	Stability  Stability
	Workstream string
	Brief      string
}

// SCMMetricConventions documents the six GitHub-sync metrics the operator emits
// on telemetry.Meter() (GH-1..3). All are Stable — scm_conformance_test.go
// drives the real pkg/telemetry/scmmetrics emitter and asserts each one is
// produced with these labels.
var SCMMetricConventions = []MetricConvention{
	{
		Name: "ksquad.scm.webhook.total", Instrument: InstrumentCounter, Unit: "1",
		Labels: []string{"event", "outcome"}, Stability: Stable, Workstream: "WS-GH",
		Brief: "SCM webhook deliveries by event and outcome (GH-1). outcome=accepted proves ingress is alive; a dropped webhook stops incrementing.",
	},
	{
		Name: "ksquad.scm.sync.total", Instrument: InstrumentCounter, Unit: "1",
		Labels: []string{"provider", "trigger", "reason"}, Stability: Stable, Workstream: "WS-GH",
		Brief: "SCM reconcile passes by provider, trigger and reason (GH-3). The reason label reuses the SyncReady condition taxonomy so success rate = Synced / total.",
	},
	{
		Name: "ksquad.scm.sync.duration", Instrument: InstrumentHistogram, Unit: "s",
		Labels: []string{"provider", "trigger"}, Stability: Stable, Workstream: "WS-GH",
		Brief: "SCM reconcile pass latency in seconds (GH-3). Freshness SLO source: p99 should stay under pollInterval+60s.",
	},
	{
		Name: "ksquad.scm.sync.panics.total", Instrument: InstrumentCounter, Unit: "1",
		Labels: []string{"provider"}, Stability: Stable, Workstream: "WS-GH",
		Brief: "SCM reconcile panics recovered and re-raised (GH-2). The nil Client/Providers/Store deref SLO: this must stay 0.",
	},
	{
		Name: "ksquad.scm.mirror.age", Instrument: InstrumentObservableGauge, Unit: "s",
		Labels: []string{"project"}, Stability: Stable, Workstream: "WS-GH",
		Brief: "Seconds since each Project's last successful mirror pass (GH-3). A stalled reconcile shows an ever-growing age rather than dropping off the series.",
	},
	{
		Name: "ksquad.scm.provider.rate_limit.remaining", Instrument: InstrumentObservableGauge, Unit: "1",
		Labels: []string{"provider"}, Stability: Stable, Workstream: "WS-GH",
		Brief: "Last-seen provider rate-limit headroom (requests remaining) by provider (GH-3). Approaching 0 explains stale mirrors that are not errors.",
	},
}

// DocumentedSCMMetricNames returns the set of every SCM metric name the registry
// documents. scm_conformance_test.go uses it as the no-undocumented-metric
// regression guard against the real emitter.
func DocumentedSCMMetricNames() map[string]bool {
	names := map[string]bool{}
	for _, m := range SCMMetricConventions {
		names[m.Name] = true
	}
	return names
}

// StableSCMMetricNames returns the SCM metric names the emitter MUST produce
// today (Stability Stable).
func StableSCMMetricNames() []string {
	var out []string
	for _, m := range SCMMetricConventions {
		if m.Stability == Stable {
			out = append(out, m.Name)
		}
	}
	return out
}
