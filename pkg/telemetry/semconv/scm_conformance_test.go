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

// SCM (GitHub-sync) semconv conformance — ISI-4395 GH-5 / WS-GH. Unlike the
// run-trace suite in conformance_test.go (which drives the toolusage.Mapper),
// the SCM metrics are emitted by pkg/telemetry/scmmetrics, so this suite drives
// that REAL emitter through a ManualReader and asserts:
//
//   - every Stable metric the registry documents is actually produced
//     (registry ⊆ emitter — flip a convention to Stable and it starts enforcing);
//   - no ksquad.scm.* metric the emitter produces is undocumented (the
//     no-drift regression guard);
//   - each emitted metric is dimensioned only by its documented labels (so a
//     high-cardinality repo/run.id label can never sneak in unnoticed).
//
// The SCM spans are emitted by three separate processes (scm-webhook, operator,
// apiserver); driving all of them here would pull heavy deps into the contract
// package, so their conventions are validated for self-consistency instead.
package ksqsemconv_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/K8squad/K8squad/pkg/telemetry/scmmetrics"
	ksqsemconv "github.com/K8squad/K8squad/pkg/telemetry/semconv"
)

// emitAllSCMMetrics registers the real scm emitter, exercises every recording
// path so all six instruments (including the two observable gauges) have data,
// and returns the collected metrics keyed by name plus, per metric, the union
// of attribute (label) keys seen across its data points.
func emitAllSCMMetrics(t *testing.T) (names map[string]bool, labelsByMetric map[string]map[string]bool) {
	t.Helper()

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	m, err := scmmetrics.Register(mp.Meter("scm-conformance"))
	if err != nil {
		t.Fatalf("scmmetrics.Register: %v", err)
	}

	ctx := context.Background()
	m.RecordWebhook(ctx, "push", scmmetrics.OutcomeAccepted)
	m.RecordSync(ctx, scmmetrics.SyncOutcome{
		Provider: "github",
		Trigger:  scmmetrics.TriggerWebhook,
		Reason:   "Synced",
		Project:  "team-a/proj-1",
		Duration: 1200 * time.Millisecond,
		Success:  true, // stamps the mirror-age gauge for the project
	})
	m.RecordPanic(ctx, "github")
	m.ObserveRateLimit("github", 4821)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}

	names = map[string]bool{}
	labelsByMetric = map[string]map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			names[md.Name] = true
			labelsByMetric[md.Name] = dataPointKeys(md.Data)
		}
	}
	return names, labelsByMetric
}

// dataPointKeys returns the union of attribute keys across all data points of a
// metric, regardless of instrument kind.
func dataPointKeys(data metricdata.Aggregation) map[string]bool {
	keys := map[string]bool{}
	switch d := data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			addSetKeys(keys, dp.Attributes.ToSlice())
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			addSetKeys(keys, dp.Attributes.ToSlice())
		}
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			addSetKeys(keys, dp.Attributes.ToSlice())
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			addSetKeys(keys, dp.Attributes.ToSlice())
		}
	case metricdata.Histogram[int64]:
		for _, dp := range d.DataPoints {
			addSetKeys(keys, dp.Attributes.ToSlice())
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			addSetKeys(keys, dp.Attributes.ToSlice())
		}
	}
	return keys
}

func addSetKeys(dst map[string]bool, kvs []attribute.KeyValue) {
	for _, kv := range kvs {
		dst[string(kv.Key)] = true
	}
}

// TestSCMConformance_StableMetricsEmitted asserts every Stable metric the
// registry documents is actually produced by the real emitter.
func TestSCMConformance_StableMetricsEmitted(t *testing.T) {
	names, _ := emitAllSCMMetrics(t)
	for _, want := range ksqsemconv.StableSCMMetricNames() {
		if !names[want] {
			t.Errorf("registry marks %q Stable but the scmmetrics emitter did not produce it", want)
		}
	}
}

// TestSCMConformance_NoUndocumentedMetrics is the no-drift guard: any
// ksquad.scm.* metric the emitter produces must be documented in the registry.
func TestSCMConformance_NoUndocumentedMetrics(t *testing.T) {
	names, _ := emitAllSCMMetrics(t)
	documented := ksqsemconv.DocumentedSCMMetricNames()
	for name := range names {
		if !strings.HasPrefix(name, "ksquad.scm.") {
			continue
		}
		if !documented[name] {
			t.Errorf("emitter produces UNDOCUMENTED metric %q — add it to pkg/telemetry/semconv/scm.go", name)
		}
	}
}

// TestSCMConformance_MetricLabelsMatch asserts each emitted metric is
// dimensioned by exactly its documented label set — the cardinality guard that
// would catch a stray repo/run.id label.
func TestSCMConformance_MetricLabelsMatch(t *testing.T) {
	_, labels := emitAllSCMMetrics(t)
	for _, mc := range ksqsemconv.SCMMetricConventions {
		got, ok := labels[mc.Name]
		if !ok {
			continue // absence is covered by TestSCMConformance_StableMetricsEmitted
		}
		want := map[string]bool{}
		for _, l := range mc.Labels {
			want[l] = true
		}
		for k := range got {
			if !want[k] {
				t.Errorf("metric %q emits undocumented label %q (documented labels: %v)", mc.Name, k, mc.Labels)
			}
		}
		for l := range want {
			if !got[l] {
				t.Errorf("metric %q missing documented label %q (got %v)", mc.Name, l, keysSorted(got))
			}
		}
	}
}

// TestSCMConformance_SpansWellFormed validates the SCM span conventions are
// self-consistent: unique names, at least one attribute each, and every key in
// the scm.* or ksquad.scm.* namespace with a real type/requirement/stability.
func TestSCMConformance_SpansWellFormed(t *testing.T) {
	validType := map[ksqsemconv.AttrType]bool{
		ksqsemconv.TypeString: true, ksqsemconv.TypeInt: true, ksqsemconv.TypeDouble: true,
		ksqsemconv.TypeBool: true, ksqsemconv.TypeStringSlice: true,
	}
	validReq := map[ksqsemconv.Requirement]bool{
		ksqsemconv.Required: true, ksqsemconv.Conditional: true, ksqsemconv.Recommended: true,
	}
	seen := map[string]bool{}
	for _, sc := range ksqsemconv.SCMSpanConventions {
		if seen[sc.Name] {
			t.Errorf("duplicate SCM span convention %q", sc.Name)
		}
		seen[sc.Name] = true
		if len(sc.Attributes) == 0 {
			t.Errorf("SCM span %q documents no attributes", sc.Name)
		}
		for _, a := range sc.Attributes {
			if !strings.HasPrefix(a.Key, "scm.") && !strings.HasPrefix(a.Key, "ksquad.scm.") &&
				!isStandardOTelSemconvKey(a.Key) {
				t.Errorf("SCM span %q attribute %q is not in the scm.*/ksquad.scm.* namespace (or a standard OTel semconv key)", sc.Name, a.Key)
			}
			if !validType[a.Type] {
				t.Errorf("SCM span %q attribute %q has invalid type %q", sc.Name, a.Key, a.Type)
			}
			if !validReq[a.Requirement] {
				t.Errorf("SCM span %q attribute %q has invalid requirement %q", sc.Name, a.Key, a.Requirement)
			}
			if a.Stability != ksqsemconv.Stable && a.Stability != ksqsemconv.Planned {
				t.Errorf("SCM span %q attribute %q has invalid stability %q", sc.Name, a.Key, a.Stability)
			}
		}
	}
}

func keysSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// isStandardOTelSemconvKey reports whether key is a standard OpenTelemetry
// HTTP/network semantic-convention key (rather than a K8squad domain key). The
// scm.fetch.<kind> span carries OTel HTTP client semantics (method/path/
// server.address/status — ISI-5013) in addition to the ksquad.scm.* domain
// attributes, so those standard namespaces are legitimate here.
func isStandardOTelSemconvKey(key string) bool {
	for _, ns := range []string{"http.", "url.", "server.", "client.", "network."} {
		if strings.HasPrefix(key, ns) {
			return true
		}
	}
	return false
}
