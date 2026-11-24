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

// Package instscoreingest is the apiserver half of the instrumentation-score
// ingestion tap (ISI-7749, phase 3 of the ISI-7741 plan): it receives the OTel
// gateway's mirrored OTLP batches over HTTP, splits them per service.name,
// persists per-service evidence windows in Postgres (migration 0042), and
// re-evaluates the scoring engine over the stored window so the phase-4 read
// surface always serves the latest verdicts.
package instscoreingest

import (
	"fmt"
	"sort"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	logscolpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricscolpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecolpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// The splitter carves one collector batch into per-service batches. A gateway
// batch routinely carries many services' resource groups; the evidence window
// is per service, so each service's rows contain ONLY its resource groups —
// the phase-4 window read folds a service's history without touching anyone
// else's telemetry. Grouping follows the engine exactly: by the resource's
// service.name, with a missing/blank value folding into "" (RES-005 evidence).

// serviceNameOf returns the resource's service.name ("" when absent or not a
// string) — the same grouping key pkg/telemetry/instscore's Accumulator uses.
func serviceNameOf(r *resourcepb.Resource) string {
	for _, kv := range r.GetAttributes() {
		if kv.GetKey() == "service.name" {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// resourceKey renders a resource attribute set as a sorted key=value string —
// the same identity the engine's fingerprint uses, recomputed locally so this
// package does not reach into unexported engine internals.
func resourceKey(r *resourcepb.Resource) string {
	parts := make([]string, 0, len(r.GetAttributes()))
	for _, kv := range r.GetAttributes() {
		parts = append(parts, kv.GetKey()+"="+anyValueKey(kv.GetValue()))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func anyValueKey(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return fmt.Sprintf("%q", x.StringValue)
	case *commonpb.AnyValue_BoolValue:
		return fmt.Sprintf("%v", x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprintf("%d", x.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprintf("%g", x.DoubleValue)
	case *commonpb.AnyValue_BytesValue:
		return fmt.Sprintf("bytes:%x", x.BytesValue)
	default:
		return "<empty>"
	}
}

// TraceSplit is one service's slice of a trace export request.
type TraceSplit struct {
	ServiceName string
	Request     *tracecolpb.ExportTraceServiceRequest
	Spans       int
	Resources   int
}

// SplitTraces splits an export request into per-service export requests,
// preserving each resource group's scope spans verbatim.
func SplitTraces(req *tracecolpb.ExportTraceServiceRequest) []TraceSplit {
	if req == nil {
		return nil
	}
	byService := map[string]*tracecolpb.ExportTraceServiceRequest{}
	seenResource := map[string]map[string]bool{}
	spans := map[string]int{}
	var order []string
	for _, rs := range req.ResourceSpans {
		name := serviceNameOf(rs.GetResource())
		out, ok := byService[name]
		if !ok {
			out = &tracecolpb.ExportTraceServiceRequest{}
			byService[name] = out
			seenResource[name] = map[string]bool{}
			order = append(order, name)
		}
		n := 0
		for _, ss := range rs.ScopeSpans {
			n += len(ss.GetSpans())
		}
		out.ResourceSpans = append(out.ResourceSpans, rs)
		spans[name] += n
		seenResource[name][resourceKey(rs.GetResource())] = true
	}
	splits := make([]TraceSplit, 0, len(order))
	for _, name := range order {
		splits = append(splits, TraceSplit{
			ServiceName: name,
			Request:     byService[name],
			Spans:       spans[name],
			Resources:   len(seenResource[name]),
		})
	}
	return splits
}

// MetricSplit is one service's slice of a metrics export request.
type MetricSplit struct {
	ServiceName string
	Request     *metricscolpb.ExportMetricsServiceRequest
	Metrics     int
	Resources   int
}

// SplitMetrics splits a metrics export request per service.name.
func SplitMetrics(req *metricscolpb.ExportMetricsServiceRequest) []MetricSplit {
	if req == nil {
		return nil
	}
	byService := map[string]*metricscolpb.ExportMetricsServiceRequest{}
	seenResource := map[string]map[string]bool{}
	metrics := map[string]int{}
	var order []string
	for _, rm := range req.ResourceMetrics {
		name := serviceNameOf(rm.GetResource())
		out, ok := byService[name]
		if !ok {
			out = &metricscolpb.ExportMetricsServiceRequest{}
			byService[name] = out
			seenResource[name] = map[string]bool{}
			order = append(order, name)
		}
		n := 0
		for _, sm := range rm.ScopeMetrics {
			n += len(sm.GetMetrics())
		}
		out.ResourceMetrics = append(out.ResourceMetrics, rm)
		metrics[name] += n
		seenResource[name][resourceKey(rm.GetResource())] = true
	}
	splits := make([]MetricSplit, 0, len(order))
	for _, name := range order {
		splits = append(splits, MetricSplit{
			ServiceName: name,
			Request:     byService[name],
			Metrics:     metrics[name],
			Resources:   len(seenResource[name]),
		})
	}
	return splits
}

// LogSplit is one service's slice of a logs export request.
type LogSplit struct {
	ServiceName string
	Request     *logscolpb.ExportLogsServiceRequest
	Logs        int
	Resources   int
}

// SplitLogs splits a logs export request per service.name.
func SplitLogs(req *logscolpb.ExportLogsServiceRequest) []LogSplit {
	if req == nil {
		return nil
	}
	byService := map[string]*logscolpb.ExportLogsServiceRequest{}
	seenResource := map[string]map[string]bool{}
	logs := map[string]int{}
	var order []string
	for _, rl := range req.ResourceLogs {
		name := serviceNameOf(rl.GetResource())
		out, ok := byService[name]
		if !ok {
			out = &logscolpb.ExportLogsServiceRequest{}
			byService[name] = out
			seenResource[name] = map[string]bool{}
			order = append(order, name)
		}
		n := 0
		for _, sl := range rl.ScopeLogs {
			n += len(sl.GetLogRecords())
		}
		out.ResourceLogs = append(out.ResourceLogs, rl)
		logs[name] += n
		seenResource[name][resourceKey(rl.GetResource())] = true
	}
	splits := make([]LogSplit, 0, len(order))
	for _, name := range order {
		splits = append(splits, LogSplit{
			ServiceName: name,
			Request:     byService[name],
			Logs:        logs[name],
			Resources:   len(seenResource[name]),
		})
	}
	return splits
}
