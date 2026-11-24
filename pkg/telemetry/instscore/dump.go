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
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	logscolpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricscolpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecolpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// An OTLP JSON dump is the offline input of the scoring engine and the
// cmd/instscore CLI: telemetry captured at the collector wire boundary,
// encoded with protojson. Two shapes are accepted:
//
//   - multi-signal: {"traces": [...], "metrics": [...], "logs": [...]} where
//     each array element is an Export{Trace,Metrics,Logs}ServiceRequest;
//     every key is optional.
//   - single-signal: a bare Export*ServiceRequest object (detected by its
//     resourceSpans / resourceMetrics / resourceLogs field).
//
// Unknown fields are discarded, so dumps from collectors carrying extra
// annotations still load.

// LoadDump folds an OTLP JSON dump into a fresh accumulator.
func LoadDump(data []byte) (*Accumulator, error) {
	acc := NewAccumulator()
	if err := LoadDumpInto(acc, data); err != nil {
		return nil, err
	}
	return acc, nil
}

// LoadDumpInto folds an OTLP JSON dump into an existing accumulator, so
// multi-file dumps merge under one set of per-service windows.
func LoadDumpInto(acc *Accumulator, data []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return fmt.Errorf("dump is not a JSON object: %w", err)
	}

	if raw, ok := top["traces"]; ok {
		var reqs []json.RawMessage
		if err := json.Unmarshal(raw, &reqs); err != nil {
			return fmt.Errorf("traces: %w", err)
		}
		for i, r := range reqs {
			req := &tracecolpb.ExportTraceServiceRequest{}
			if err := protoUnmarshal(r, req); err != nil {
				return fmt.Errorf("traces[%d]: %w", i, err)
			}
			acc.AcceptTraces(req)
		}
	}
	if raw, ok := top["metrics"]; ok {
		var reqs []json.RawMessage
		if err := json.Unmarshal(raw, &reqs); err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		for i, r := range reqs {
			req := &metricscolpb.ExportMetricsServiceRequest{}
			if err := protoUnmarshal(r, req); err != nil {
				return fmt.Errorf("metrics[%d]: %w", i, err)
			}
			acc.AcceptMetrics(req)
		}
	}
	if raw, ok := top["logs"]; ok {
		var reqs []json.RawMessage
		if err := json.Unmarshal(raw, &reqs); err != nil {
			return fmt.Errorf("logs: %w", err)
		}
		for i, r := range reqs {
			req := &logscolpb.ExportLogsServiceRequest{}
			if err := protoUnmarshal(r, req); err != nil {
				return fmt.Errorf("logs[%d]: %w", i, err)
			}
			acc.AcceptLogs(req)
		}
	}

	// Single-signal bare request: the whole document is one export request.
	if len(acc.windows) == 0 {
		switch {
		case top["resourceSpans"] != nil || top["resource_spans"] != nil:
			req := &tracecolpb.ExportTraceServiceRequest{}
			if err := protoUnmarshal(data, req); err != nil {
				return fmt.Errorf("trace request: %w", err)
			}
			acc.AcceptTraces(req)
		case top["resourceMetrics"] != nil || top["resource_metrics"] != nil:
			req := &metricscolpb.ExportMetricsServiceRequest{}
			if err := protoUnmarshal(data, req); err != nil {
				return fmt.Errorf("metrics request: %w", err)
			}
			acc.AcceptMetrics(req)
		case top["resourceLogs"] != nil || top["resource_logs"] != nil:
			req := &logscolpb.ExportLogsServiceRequest{}
			if err := protoUnmarshal(data, req); err != nil {
				return fmt.Errorf("logs request: %w", err)
			}
			acc.AcceptLogs(req)
		case len(top) == 0:
			return fmt.Errorf("dump is empty")
		default:
			return fmt.Errorf("dump has no traces/metrics/logs or resourceSpans/resourceMetrics/resourceLogs fields")
		}
	}

	return nil
}

// protoUnmarshal decodes one protojson message, discarding unknown fields
// and tolerating both camelCase and snake_case field names.
func protoUnmarshal(data []byte, m proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(data, m)
}
