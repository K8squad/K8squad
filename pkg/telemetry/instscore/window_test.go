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

package instscore

import (
	"testing"

	logscolpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricscolpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecolpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// ---- batch builders (OTLP wire level) ----

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func kvInt(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func res(attrs ...*commonpb.KeyValue) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: attrs}
}

// goodResource is a resource that passes every RES rule it touches.
func goodResource(name, instanceID, podName, podUID string) *resourcepb.Resource {
	attrs := []*commonpb.KeyValue{
		kv("service.name", name),
		kv("service.version", "1.2.3"),
		kv("service.instance.id", instanceID),
		kv("service.criticality", "high"),
		kv("deployment.environment.name", "production"),
	}
	if podUID != "" {
		attrs = append(attrs, kv("k8s.namespace.name", "default"), kv("k8s.pod.name", podName), kv("k8s.pod.uid", podUID))
	}
	return res(attrs...)
}

func traceReq(rsps ...*tracepb.ResourceSpans) *tracecolpb.ExportTraceServiceRequest {
	return &tracecolpb.ExportTraceServiceRequest{ResourceSpans: rsps}
}

func spansReq(r *resourcepb.Resource, spans ...*tracepb.Span) *tracepb.ResourceSpans {
	return &tracepb.ResourceSpans{
		Resource:   r,
		ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "github.com/k8squad/test", Version: "v1.0.0"}, Spans: spans}},
	}
}

func span(traceID, spanID, parentID string, kind tracepb.Span_SpanKind, name string, start, end uint64, attrs ...*commonpb.KeyValue) *tracepb.Span {
	return &tracepb.Span{
		TraceId:           []byte(traceID),
		SpanId:            []byte(spanID),
		ParentSpanId:      []byte(parentID),
		Kind:              kind,
		Name:              name,
		StartTimeUnixNano: start,
		EndTimeUnixNano:   end,
		Attributes:        attrs,
	}
}

func logsReq(r *resourcepb.Resource, records ...*logspb.LogRecord) *logspb.ResourceLogs {
	return &logspb.ResourceLogs{
		Resource:  r,
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "github.com/k8squad/test", Version: "v1.0.0"}, LogRecords: records}},
	}
}

func logReq2(r *logspb.ResourceLogs) *logscolpb.ExportLogsServiceRequest {
	return &logscolpb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{r}}
}

func logRecord(timeNano uint64, severityText string, severityNumber logspb.SeverityNumber, body string, attrs ...*commonpb.KeyValue) *logspb.LogRecord {
	return &logspb.LogRecord{
		TimeUnixNano:   timeNano,
		SeverityText:   severityText,
		SeverityNumber: severityNumber,
		Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		Attributes:     attrs,
	}
}

func metricsReq(rms ...*metricspb.ResourceMetrics) *metricscolpb.ExportMetricsServiceRequest {
	return &metricscolpb.ExportMetricsServiceRequest{ResourceMetrics: rms}
}

func metricsReqFor(r *resourcepb.Resource, ms ...*metricspb.Metric) *metricspb.ResourceMetrics {
	return &metricspb.ResourceMetrics{
		Resource:     r,
		ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "github.com/k8squad/test", Version: "v1.0.0"}, Metrics: ms}},
	}
}

func gaugeMetric(name, unit string, t uint64, attrs ...*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Unit: unit,
		Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
			TimeUnixNano: t,
			Attributes:   attrs,
			Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 1},
		}}}},
	}
}

func histMetric(name, unit string, t uint64, bounds []float64) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Unit: unit,
		Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: []*metricspb.HistogramDataPoint{{
			TimeUnixNano:   t,
			ExplicitBounds: bounds,
			BucketCounts:   make([]uint64, len(bounds)+1),
		}}},
		},
	}
}

// ns converts seconds to unix nanos for readable fixture times.
func ns(sec uint64) uint64 { return sec * 1_000_000_000 }

// evaluateRules folds batches and evaluates one rule, returning its verdict —
// the per-rule table test harness.
func evaluateRule(t *testing.T, id string, fold func(*Accumulator)) Verdict {
	t.Helper()
	acc := NewAccumulator()
	fold(acc)
	var target *Window
	for _, w := range acc.Windows() {
		if w.ServiceName == "svc" {
			target = w
			break
		}
	}
	if target == nil {
		ws := acc.Windows()
		if len(ws) == 0 {
			t.Fatalf("no window accumulated for rule %s", id)
		}
		target = ws[0]
	}
	eval, ok := evaluators[id]
	if !ok {
		t.Fatalf("rule %s has no evaluator", id)
	}
	return eval(target)
}

// ---- window tests ----

func TestAccumulatorGroupsByService(t *testing.T) {
	acc := NewAccumulator()
	acc.AcceptTraces(traceReq(spansReq(goodResource("payments", "i-1", "pod-1", "uid-1"),
		span("tr1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "GET /pay", ns(100), ns(105)))))
	acc.AcceptLogs(logReq2(logsReq(goodResource("checkout", "i-2", "pod-2", "uid-2"),
		logRecord(ns(100), "INFO", 9, "hello"))))
	acc.AcceptMetrics(metricsReq(metricsReqFor(goodResource("payments", "i-1", "pod-1", "uid-1"),
		gaugeMetric("queue.depth", "{obj}", ns(100)))))

	ws := acc.Windows()
	if len(ws) != 2 {
		t.Fatalf("want 2 windows, got %d", len(ws))
	}
	if ws[0].ServiceName != "payments" || ws[1].ServiceName != "checkout" {
		t.Fatalf("window order/names: %q, %q", ws[0].ServiceName, ws[1].ServiceName)
	}
	if len(ws[0].Spans) != 1 || len(ws[0].Metrics) != 1 || len(ws[0].Logs) != 0 {
		t.Fatalf("payments window contents wrong: spans=%d metrics=%d logs=%d", len(ws[0].Spans), len(ws[0].Metrics), len(ws[0].Logs))
	}
	if len(ws[1].Logs) != 1 {
		t.Fatalf("checkout window logs: %d", len(ws[1].Logs))
	}
}

func TestAccumulatorDedupesResources(t *testing.T) {
	acc := NewAccumulator()
	r := goodResource("svc", "i-1", "pod-1", "uid-1")
	acc.AcceptTraces(traceReq(spansReq(r, span("tr1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "a", ns(1), ns(2)))))
	acc.AcceptTraces(traceReq(spansReq(r, span("tr1", "s2", "", tracepb.Span_SPAN_KIND_SERVER, "b", ns(1), ns(2)))))
	w := acc.Window("svc")
	if len(w.Resources) != 1 {
		t.Fatalf("want 1 deduped resource, got %d", len(w.Resources))
	}
	if len(w.Spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(w.Spans))
	}
}

func TestAccumulatorUnnamedService(t *testing.T) {
	acc := NewAccumulator()
	acc.AcceptTraces(traceReq(spansReq(res(kv("service.version", "1.0.0")),
		span("tr1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "a", ns(1), ns(2)))))
	w := acc.Window("")
	if w == nil {
		t.Fatal("unnamed window missing")
	}
	if w.ServiceName != "" || len(w.Resources) != 1 {
		t.Fatalf("unnamed window wrong: %+v", w)
	}
}

func TestHasSignal(t *testing.T) {
	acc := NewAccumulator()
	acc.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"), gaugeMetric("m", "1", ns(1)))))
	w := acc.Window("svc")
	if !w.HasSignal(TargetMetric) || !w.HasSignal(TargetResource) {
		t.Fatal("metrics+resource should be present")
	}
	if w.HasSignal(TargetSpan) || w.HasSignal(TargetLog) {
		t.Fatal("spans/logs should be absent")
	}
}
