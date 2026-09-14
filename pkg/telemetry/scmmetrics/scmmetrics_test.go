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

package scmmetrics

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTestMeter(t *testing.T) (metric.Meter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return mp.Meter("scmmetrics-test"), reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func attrStr(set attribute.Set, key string) string {
	v, _ := set.Value(attribute.Key(key))
	return v.AsString()
}

// TestRegisterCreatesInstruments is the direct undiagnosability regression: the
// scm path exported NO metrics before this package. After Register the meter's
// reader must yield the sync counter, duration histogram, panics counter and
// webhook counter (the gauges appear only once they have observations).
func TestRegisterCreatesInstruments(t *testing.T) {
	meter, reader := newTestMeter(t)
	m, err := Register(meter)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx := context.Background()
	m.RecordWebhook(ctx, "push", OutcomeAccepted)
	m.RecordSync(ctx, SyncOutcome{Provider: "github", Trigger: TriggerWebhook, Reason: "Synced", Project: "ns/app", Duration: 250 * time.Millisecond, Success: true})
	m.RecordPanic(ctx, "github")

	got := collect(t, reader)
	for _, name := range []string{webhookTotalName, syncTotalName, syncDurationName, syncPanicsName} {
		if _, ok := got[name]; !ok {
			t.Errorf("instrument %q not registered/collected", name)
		}
	}
}

// TestWebhookCounterLabels checks the {event,outcome} label set the dashboard
// slices "webhook accepted > 0" on.
func TestWebhookCounterLabels(t *testing.T) {
	meter, reader := newTestMeter(t)
	m, _ := Register(meter)
	ctx := context.Background()
	m.RecordWebhook(ctx, "push", OutcomeAccepted)
	m.RecordWebhook(ctx, "push", OutcomeAccepted)
	m.RecordWebhook(ctx, "", OutcomeRejected) // empty event folds to "unknown"

	sum, ok := collect(t, reader)[webhookTotalName].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("webhook total is not an int64 sum")
	}
	seen := map[string]int64{}
	for _, dp := range sum.DataPoints {
		seen[attrStr(dp.Attributes, attrEvent)+"/"+attrStr(dp.Attributes, attrOutcome)] = dp.Value
	}
	if seen["push/"+OutcomeAccepted] != 2 {
		t.Errorf("push/accepted = %d, want 2", seen["push/"+OutcomeAccepted])
	}
	if seen["unknown/"+OutcomeRejected] != 1 {
		t.Errorf("unknown/rejected = %d, want 1 (empty event should fold to unknown)", seen["unknown/"+OutcomeRejected])
	}
}

// TestSyncCounterReasonTaxonomy verifies the {provider,trigger,reason} labels
// carry the reposync reason taxonomy verbatim.
func TestSyncCounterReasonTaxonomy(t *testing.T) {
	meter, reader := newTestMeter(t)
	m, _ := Register(meter)
	ctx := context.Background()
	m.RecordSync(ctx, SyncOutcome{Provider: "github", Trigger: TriggerPoll, Reason: "ProviderError", Duration: time.Second})
	m.RecordSync(ctx, SyncOutcome{Provider: "github", Trigger: TriggerWebhook, Reason: "Synced", Project: "ns/app", Success: true, Duration: time.Second})

	sum := collect(t, reader)[syncTotalName].Data.(metricdata.Sum[int64])
	seen := map[string]bool{}
	for _, dp := range sum.DataPoints {
		seen[attrStr(dp.Attributes, attrProvider)+"/"+attrStr(dp.Attributes, attrTrigger)+"/"+attrStr(dp.Attributes, attrReason)] = true
	}
	if !seen["github/"+TriggerPoll+"/ProviderError"] || !seen["github/"+TriggerWebhook+"/Synced"] {
		t.Errorf("missing expected {provider,trigger,reason} series: got %v", seen)
	}
}

// TestMirrorAgeGaugeTracksSuccess is the freshness SLI: a successful pass stamps
// a mirror-age series for the project, and the age grows with wall time. A pass
// that never succeeded produces no series (age is meaningless).
func TestMirrorAgeGaugeTracksSuccess(t *testing.T) {
	meter, reader := newTestMeter(t)
	m, _ := Register(meter)
	base := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return base }
	m.RecordSync(context.Background(), SyncOutcome{Provider: "github", Trigger: TriggerPoll, Reason: "Synced", Project: "ns/app", Success: true, Duration: time.Second})

	m.now = func() time.Time { return base.Add(90 * time.Second) }
	g, ok := collect(t, reader)[mirrorAgeName].Data.(metricdata.Gauge[float64])
	if !ok {
		t.Fatalf("mirror age is not a float64 gauge")
	}
	if len(g.DataPoints) != 1 {
		t.Fatalf("mirror age points = %d, want 1", len(g.DataPoints))
	}
	if got := g.DataPoints[0].Value; got < 89 || got > 91 {
		t.Errorf("mirror age = %.1fs, want ~90s", got)
	}
	if p := attrStr(g.DataPoints[0].Attributes, attrProject); p != "ns/app" {
		t.Errorf("mirror age project label = %q, want ns/app", p)
	}
}

// TestMirrorAgeSkipsFailedPass proves a failed sync does not create/refresh the
// freshness series — otherwise a broken mirror would masquerade as "just synced".
func TestMirrorAgeSkipsFailedPass(t *testing.T) {
	meter, reader := newTestMeter(t)
	m, _ := Register(meter)
	m.RecordSync(context.Background(), SyncOutcome{Provider: "github", Trigger: TriggerPoll, Reason: "ProviderError", Project: "ns/app", Success: false, Duration: time.Second})
	if _, ok := collect(t, reader)[mirrorAgeName]; ok {
		t.Errorf("mirror-age series present after a failed pass; want none")
	}
}

// TestRateLimitGauge verifies the provider-headroom gauge reports the last-seen
// value and ignores an unknown (negative) reading.
func TestRateLimitGauge(t *testing.T) {
	meter, reader := newTestMeter(t)
	m, _ := Register(meter)
	m.ObserveRateLimit("github", 4321)
	m.ObserveRateLimit("github", -1) // unknown: must not overwrite
	m.ObserveRateLimit("github", 4000)

	g := collect(t, reader)[rateRemainingName].Data.(metricdata.Gauge[int64])
	if len(g.DataPoints) != 1 || g.DataPoints[0].Value != 4000 {
		t.Fatalf("rate gauge = %+v, want single point value 4000", g.DataPoints)
	}
}

// TestNilReceiverSafe: every recording method must no-op on a nil *Metrics so an
// un-instrumented binary path never panics.
func TestNilReceiverSafe(t *testing.T) {
	var m *Metrics
	ctx := context.Background()
	m.RecordWebhook(ctx, "push", OutcomeAccepted)
	m.RecordSync(ctx, SyncOutcome{})
	m.RecordPanic(ctx, "github")
	m.ObserveRateLimit("github", 1)
	m.ForgetProject("ns/app")
}
