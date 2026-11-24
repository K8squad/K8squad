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
	"fmt"
	"sort"
	"strings"

	logscolpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricscolpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecolpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Window is the accumulated, per-service evidence an evaluation runs against:
// every span, log record, and metric data point observed for one
// service.name, each linked to the resource (attribute set) it was exported
// with. It is built by folding OTLP export requests (collector wire format)
// through the Accumulator, which groups by the resource's service.name.
//
// A Window is plain data: no clocks, no I/O, no randomness — the same batches
// folded in the same order produce the same window, so scores are
// deterministic and CI-diffable.
type Window struct {
	// ServiceName is the service.name the window was grouped under. It is ""
	// when telemetry arrived with a missing or empty service.name (which
	// RES-005 then reports as a violation).
	ServiceName string

	// Resources holds every distinct resource attribute set observed in the
	// window, deduplicated by fingerprint, in first-seen order.
	Resources []*ResourceEvidence

	// Spans, Logs, Metrics hold the observed records, in arrival order.
	Spans   []*SpanEvidence
	Logs    []*LogEvidence
	Metrics []*MetricEvidence
}

// ResourceEvidence is one distinct resource attribute set in the window.
type ResourceEvidence struct {
	// Attributes is the resource attribute list as exported (vendored order).
	Attributes []*commonpb.KeyValue
	// Fingerprint uniquely identifies the attribute set (sorted key=value
	// pairs), used for deduplication and RES-002 logical-resource identity.
	Fingerprint string
}

// SpanEvidence is one observed span with its exporting resource and scope.
type SpanEvidence struct {
	Resource  *ResourceEvidence
	Scope     *commonpb.InstrumentationScope
	TraceID   string
	SpanID    string
	ParentID  string
	Kind      tracepb.Span_SpanKind
	Name      string
	Attrs     []*commonpb.KeyValue
	Events    []*tracepb.Span_Event
	Links     []*tracepb.Span_Link
	StatusMsg string
	StartNano uint64
	EndNano   uint64
}

// LogEvidence is one observed log record with its exporting resource and scope.
type LogEvidence struct {
	Resource       *ResourceEvidence
	Scope          *commonpb.InstrumentationScope
	TimeNano       uint64
	SeverityText   string
	SeverityNumber logspb.SeverityNumber
	Body           *commonpb.AnyValue
	Attrs          []*commonpb.KeyValue
}

// MetricEvidence is one observed metric, flattened to the resolution the rule
// evaluators need: identity (name/unit/type), histogram bucket signature, and
// the data points with their attributes and exemplars.
type MetricEvidence struct {
	Resource   *ResourceEvidence
	Scope      *commonpb.InstrumentationScope
	Name       string
	Unit       string
	Type       string // gauge | sum | histogram | exponential_histogram | summary
	BucketsSig string // explicit_bounds signature for MET-004 ("" when n/a)
	Points     []*MetricPoint
}

// MetricPoint is one data point of a metric.
type MetricPoint struct {
	TimeNano  uint64
	Attrs     []*commonpb.KeyValue
	Exemplars []*metricspb.Exemplar
}

// Accumulator folds OTLP export requests into per-service windows.
// The zero value is ready to use. Batches may arrive in any signal order;
// grouping is by service.name only.
type Accumulator struct {
	// windows preserves first-seen service order for deterministic output.
	order    []string
	windows  map[string]*Window
	resource map[string]map[string]*ResourceEvidence
}

// NewAccumulator returns an empty Accumulator.
func NewAccumulator() *Accumulator {
	return &Accumulator{
		windows:  map[string]*Window{},
		resource: map[string]map[string]*ResourceEvidence{},
	}
}

// Windows returns every accumulated window in first-seen service order.
func (a *Accumulator) Windows() []*Window {
	out := make([]*Window, 0, len(a.order))
	for _, name := range a.order {
		out = append(out, a.windows[name])
	}
	return out
}

// Window returns the accumulated window for one service.name.
func (a *Accumulator) Window(serviceName string) *Window {
	return a.windows[serviceName]
}

// AcceptTraces folds an OTLP trace export request into the accumulator.
func (a *Accumulator) AcceptTraces(req *tracecolpb.ExportTraceServiceRequest) {
	if req == nil {
		return
	}
	for _, rs := range req.ResourceSpans {
		res := a.resourceFor(rs.Resource)
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				w := a.windowFor(res)
				ev := &SpanEvidence{
					Resource:  res,
					Scope:     ss.Scope,
					TraceID:   traceKey(sp.TraceId),
					SpanID:    spanKey(sp.SpanId),
					ParentID:  spanKey(sp.ParentSpanId),
					Kind:      sp.Kind,
					Name:      sp.Name,
					Attrs:     sp.Attributes,
					Events:    sp.Events,
					Links:     sp.Links,
					StatusMsg: sp.GetStatus().GetMessage(),
					StartNano: sp.StartTimeUnixNano,
					EndNano:   sp.EndTimeUnixNano,
				}
				w.Spans = append(w.Spans, ev)
			}
		}
	}
}

// AcceptMetrics folds an OTLP metrics export request into the accumulator.
func (a *Accumulator) AcceptMetrics(req *metricscolpb.ExportMetricsServiceRequest) {
	if req == nil {
		return
	}
	for _, rm := range req.ResourceMetrics {
		res := a.resourceFor(rm.Resource)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				w := a.windowFor(res)
				ev := &MetricEvidence{
					Resource: res,
					Scope:    sm.Scope,
					Name:     m.Name,
					Unit:     m.Unit,
					Type:     metricType(m),
					Points:   metricPoints(m),
				}
				switch d := m.Data.(type) {
				case *metricspb.Metric_Histogram:
					ev.BucketsSig = histogramBucketsSig(d.Histogram)
				case *metricspb.Metric_ExponentialHistogram:
					ev.BucketsSig = fmt.Sprintf("exp:%d", d.ExponentialHistogram.GetAggregationTemporality())
				}
				w.Metrics = append(w.Metrics, ev)
			}
		}
	}
}

// AcceptLogs folds an OTLP logs export request into the accumulator.
func (a *Accumulator) AcceptLogs(req *logscolpb.ExportLogsServiceRequest) {
	if req == nil {
		return
	}
	for _, rl := range req.ResourceLogs {
		res := a.resourceFor(rl.Resource)
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				w := a.windowFor(res)
				w.Logs = append(w.Logs, &LogEvidence{
					Resource:       res,
					Scope:          sl.Scope,
					TimeNano:       lr.TimeUnixNano,
					SeverityText:   lr.SeverityText,
					SeverityNumber: lr.SeverityNumber,
					Body:           lr.Body,
					Attrs:          lr.Attributes,
				})
			}
		}
	}
}

// resourceFor deduplicates a resource by attribute fingerprint.
func (a *Accumulator) resourceFor(r *resourcepb.Resource) *ResourceEvidence {
	attrs := r.GetAttributes()
	fp := resourceFingerprint(attrs)
	serviceName := ""
	if v := attrValueString(attrs, "service.name"); v != "" {
		serviceName = v
	}
	byFp := a.resource[serviceName]
	if byFp == nil {
		byFp = map[string]*ResourceEvidence{}
		a.resource[serviceName] = byFp
	}
	if res, ok := byFp[fp]; ok {
		return res
	}
	res := &ResourceEvidence{Attributes: attrs, Fingerprint: fp}
	byFp[fp] = res
	return res
}

// windowFor returns (creating if needed) the window for the resource's
// service.name, preserving first-seen order.
func (a *Accumulator) windowFor(res *ResourceEvidence) *Window {
	serviceName := attrValueString(res.Attributes, "service.name")
	w := a.windows[serviceName]
	if w == nil {
		w = &Window{ServiceName: serviceName}
		a.windows[serviceName] = w
		a.order = append(a.order, serviceName)
	}
	// Telemetry without service.name lands in the unnamed window, where the
	// resource rules (RES-005 in particular) report the violation.
	w.Resources = appendUniqueResource(w.Resources, res)
	return w
}

func appendUniqueResource(list []*ResourceEvidence, res *ResourceEvidence) []*ResourceEvidence {
	for _, existing := range list {
		if existing.Fingerprint == res.Fingerprint {
			return list
		}
	}
	return append(list, res)
}

// resourceFingerprint renders a stable identity for an attribute set:
// sorted "key=value" pairs, values in their typed literal form.
func resourceFingerprint(attrs []*commonpb.KeyValue) string {
	parts := make([]string, 0, len(attrs))
	for _, kv := range attrs {
		parts = append(parts, kv.GetKey()+"="+anyValueString(kv.GetValue()))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// attrValueString returns the string form of an attribute, or "" when the
// attribute is absent or not string-typed.
func attrValueString(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// HasSignal reports whether the window observed any of the named targets.
// Resource targets are satisfied by any resource in the window.
func (w *Window) HasSignal(targets ...Target) bool {
	for _, t := range targets {
		switch t {
		case TargetResource:
			if len(w.Resources) > 0 {
				return true
			}
		case TargetSpan:
			if len(w.Spans) > 0 {
				return true
			}
		case TargetMetric:
			if len(w.Metrics) > 0 {
				return true
			}
		case TargetLog:
			if len(w.Logs) > 0 {
				return true
			}
		case TargetSDK:
			// The SDK target is evidence-derived (telemetry.sdk.* resource
			// attributes), so any resource carries SDK evidence.
			if len(w.Resources) > 0 {
				return true
			}
		}
	}
	return false
}

func traceKey(b []byte) string { return string(b) }
func spanKey(b []byte) string  { return string(b) }

func metricType(m *metricspb.Metric) string {
	switch m.Data.(type) {
	case *metricspb.Metric_Gauge:
		return "gauge"
	case *metricspb.Metric_Sum:
		return "sum"
	case *metricspb.Metric_Histogram:
		return "histogram"
	case *metricspb.Metric_ExponentialHistogram:
		return "exponential_histogram"
	case *metricspb.Metric_Summary:
		return "summary"
	}
	return "unknown"
}

func metricPoints(m *metricspb.Metric) []*MetricPoint {
	var dps []*metricspb.NumberDataPoint
	var hps []*metricspb.HistogramDataPoint
	switch d := m.Data.(type) {
	case *metricspb.Metric_Gauge:
		dps = d.Gauge.GetDataPoints()
	case *metricspb.Metric_Sum:
		dps = d.Sum.GetDataPoints()
	case *metricspb.Metric_Histogram:
		hps = d.Histogram.GetDataPoints()
	case *metricspb.Metric_ExponentialHistogram:
		hps = nil // bucket identity carried by BucketsSig
	case *metricspb.Metric_Summary:
		return nil
	}
	out := make([]*MetricPoint, 0, len(dps)+len(hps))
	for _, dp := range dps {
		out = append(out, &MetricPoint{TimeNano: dp.TimeUnixNano, Attrs: dp.Attributes, Exemplars: dp.Exemplars})
	}
	for _, hp := range hps {
		out = append(out, &MetricPoint{TimeNano: hp.TimeUnixNano, Attrs: hp.Attributes, Exemplars: hp.Exemplars})
	}
	return out
}

// histogramBucketsSig renders the bucket layout of a histogram metric as the
// sorted union of its data points' explicit-bounds signatures. Two metrics
// with the same name match on this signature iff they use the same bucket
// layouts (MET-004); a metric whose own data points disagree carries the
// disagreement in its signature.
func histogramBucketsSig(h *metricspb.Histogram) string {
	sigs := map[string]struct{}{}
	for _, dp := range h.GetDataPoints() {
		sigs[bucketsSig(dp.GetExplicitBounds())] = struct{}{}
	}
	return strings.Join(sortedStrings(sigs), "|")
}

func bucketsSig(bounds []float64) string {
	parts := make([]string, 0, len(bounds))
	for _, b := range bounds {
		parts = append(parts, fmt.Sprintf("%g", b))
	}
	return strings.Join(parts, ",")
}

func sortedStrings(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// anyValueString renders an OTLP scalar value in its typed literal form —
// integers in decimal, bools as true/false, strings quoted — recursing into
// arrays and kvlists. Used for fingerprints and cardinality value identity.
func anyValueString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return fmt.Sprintf("%q", x.StringValue)
	case *commonpb.AnyValue_BoolValue:
		return fmt.Sprintf("%v", x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprintf("%d", x.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprintf("%g", x.DoubleValue)
	case *commonpb.AnyValue_ArrayValue:
		parts := make([]string, 0, len(x.ArrayValue.GetValues()))
		for _, el := range x.ArrayValue.GetValues() {
			parts = append(parts, anyValueString(el))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case *commonpb.AnyValue_KvlistValue:
		pairs := make([]string, 0, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			pairs = append(pairs, kv.GetKey()+":"+anyValueString(kv.GetValue()))
		}
		sort.Strings(pairs)
		return "{" + strings.Join(pairs, ",") + "}"
	case *commonpb.AnyValue_BytesValue:
		return fmt.Sprintf("bytes:%x", x.BytesValue)
	}
	return "<empty>"
}
