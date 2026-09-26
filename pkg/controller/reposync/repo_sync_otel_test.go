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

package reposync

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	ksquadapi "github.com/K8squad/K8squad/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/K8squad/K8squad/pkg/telemetry/scmmetrics"
)

func testMetrics(t *testing.T) (*scmmetrics.Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := scmmetrics.Register(mp.Meter("reposync-otel-test"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return m, reader
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
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

// TestClassifyTrigger covers the webhook-vs-poll heuristic that labels the
// scm.sync span + ksquad_scm_sync_total{trigger}.
func TestClassifyTrigger(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	withAnnotation := func(ts *time.Time, lastMirror *time.Time) *ksquadapi.Project {
		p := syncProject(300)
		if ts != nil {
			p.Annotations = map[string]string{TriggerAnnotation: ts.Format(time.RFC3339Nano)}
		}
		if lastMirror != nil {
			p.Status.Sync = &ksquadapi.ProjectSyncStatus{LastMirrorTime: &metav1.Time{Time: *lastMirror}}
		}
		return p
	}
	newer := base.Add(time.Minute)
	older := base.Add(-time.Minute)

	cases := []struct {
		name    string
		project *ksquadapi.Project
		want    string
	}{
		{"no annotation is poll", withAnnotation(nil, nil), scmmetrics.TriggerPoll},
		{"annotation but never mirrored is webhook", withAnnotation(&base, nil), scmmetrics.TriggerWebhook},
		{"trigger newer than last mirror is webhook", withAnnotation(&newer, &base), scmmetrics.TriggerWebhook},
		{"trigger older than last mirror is poll", withAnnotation(&older, &base), scmmetrics.TriggerPoll},
		{"trigger equal to last mirror is poll", withAnnotation(&base, &base), scmmetrics.TriggerPoll},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyTrigger(tc.project); got != tc.want {
				t.Errorf("classifyTrigger = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReconcileRecordsSyncMetric: a healthy pass increments
// ksquad_scm_sync_total{reason=Synced} and stamps the mirror-age gauge.
func TestReconcileRecordsSyncMetric(t *testing.T) {
	provider := &fakeProvider{name: "github", snapshot: sampleRecords()}
	r, _ := newHarness(t, syncProject(300), provider)
	m, reader := testMetrics(t)
	r.Metrics = m

	if _, err := r.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := collectMetrics(t, reader)
	sum, ok := got["ksquad.scm.sync.total"].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("sync total missing/not int64 sum: %+v", got["ksquad.scm.sync.total"])
	}
	var found bool
	for _, dp := range sum.DataPoints {
		reason, _ := dp.Attributes.Value(attribute.Key("reason"))
		if reason.AsString() == "Synced" {
			found = true
		}
	}
	if !found {
		t.Errorf("no sync.total series with reason=Synced")
	}
	if _, ok := got["ksquad.scm.mirror.age"]; !ok {
		t.Errorf("mirror-age gauge not stamped after a successful pass")
	}
}

// TestReconcilePanicIsCountedAndReRaised is the GH-2 firewall: a nil-deref panic
// inside the pass is recorded on ksquad_scm_sync_panics_total AND re-raised, so
// the crash stays a crash (never swallowed) but is no longer invisible.
func TestReconcilePanicIsCountedAndReRaised(t *testing.T) {
	provider := &fakeProvider{name: "github", snapshot: sampleRecords()}
	r, _ := newHarness(t, syncProject(300), provider)
	m, reader := testMetrics(t)
	r.Metrics = m
	// Force a nil-interface deref at r.Store.ApplySnapshot — the same shape as
	// the historical nil Client/Providers/Store crashes (ISI-4113/4117).
	r.Store = nil

	func() {
		defer func() {
			if rec := recover(); rec == nil {
				t.Fatalf("expected the panic to be re-raised out of Reconcile")
			}
		}()
		_, _ = r.Reconcile(context.Background(), request())
	}()

	got := collectMetrics(t, reader)
	sum, ok := got["ksquad.scm.sync.panics.total"].Data.(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) == 0 {
		t.Fatalf("panics counter not incremented: %+v", got["ksquad.scm.sync.panics.total"])
	}
	if sum.DataPoints[0].Value < 1 {
		t.Errorf("panics counter = %d, want >= 1", sum.DataPoints[0].Value)
	}
}

// TestReconcileNilMetricsSafe: an un-instrumented reconciler (Metrics nil) must
// drive a normal pass without panicking.
func TestReconcileNilMetricsSafe(t *testing.T) {
	provider := &fakeProvider{name: "github", snapshot: sampleRecords()}
	r, store := newHarness(t, syncProject(300), provider)
	r.Metrics = nil
	if _, err := r.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile with nil Metrics: %v", err)
	}
	if len(store.Rows()) == 0 {
		t.Errorf("expected mirror rows applied even with nil Metrics")
	}
}

// TestReconcileSpanCarriesCodeAttrs pins ISI-5014 P1#5: the scm.sync span is
// stamped with code.namespace + code.function so it maps to its source.
func TestReconcileSpanCarriesCodeAttrs(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	provider := &fakeProvider{name: "github", snapshot: sampleRecords()}
	r, _ := newHarness(t, syncProject(300), provider)
	if _, err := r.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var attrs []attribute.KeyValue
	found := false
	for _, s := range exp.GetSpans() {
		if s.Name == "scm.sync" {
			attrs = s.Attributes
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a scm.sync span, got %d spans", len(exp.GetSpans()))
	}
	got := map[string]string{}
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if got["code.namespace"] != "github.com/K8squad/K8squad/pkg/controller/reposync" {
		t.Errorf("code.namespace = %q, want pkg/controller/reposync path", got["code.namespace"])
	}
	if got["code.function"] != "Reconcile" {
		t.Errorf("code.function = %q, want Reconcile", got["code.function"])
	}
}
