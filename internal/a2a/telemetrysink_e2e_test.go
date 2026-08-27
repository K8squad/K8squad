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

package a2a

import (
	"bytes"
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/require"

	"github.com/K8squad/K8squad/internal/apiserver"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

// TestTelemetrySinkProducesScrapeableSeries is the end-to-end assertion the
// ISI-3348 review demanded (finding 1): a tool/skill event fed through the
// core-side run-event path — TelemetrySink wrapping the run-events sink, the
// exact wiring the operator's Dispatcher takes when the physical dispatch
// drops in — produces ksquad_* series on a real Prometheus registry, the
// exposition parses, and the D3 read model aggregates it per agent. This is
// the operator-shaped feed chain, exercised component-for-component.
func TestTelemetrySinkProducesScrapeableSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	mapper := toolusage.NewMapper(nil, reg) // nil tracer: metrics-only, spans noop

	var forwarded []wire.Event
	inner := SinkFunc(func(_ context.Context, ev wire.Event) error {
		forwarded = append(forwarded, ev)
		return nil
	})
	sink := NewTelemetrySink(inner, mapper, toolusage.Labels{RunID: "run-1", Agent: "coder-1"})

	ok := true
	events := []wire.Event{
		{Seq: 1, A2ATaskID: "run-1", Type: wire.EventTool, Payload: wire.ToolPayload{
			Name: "shell", Phase: "start", Skill: "git",
			ArgsSHA256: "abcdef0123456789", // emitter hashed before the event left the process (finding 2)
		}},
		{Seq: 2, A2ATaskID: "run-1", Type: wire.EventTool, Payload: wire.ToolPayload{
			Name: "shell", Phase: "result", Skill: "git", OK: &ok,
		}},
		{Seq: 3, A2ATaskID: "run-1", Type: wire.EventSkillLoad, Payload: wire.SkillLoadPayload{
			Name: "git", SHA256: "deadbeef", OK: &ok,
		}},
		{Seq: 4, A2ATaskID: "run-1", Type: wire.EventTool, Payload: wire.ToolPayload{
			Name: "k8s_get", Phase: "start", Server: "k8s",
		}},
		{Seq: 5, A2ATaskID: "run-1", Type: wire.EventTool, Payload: wire.ToolPayload{
			Name: "k8s_get", Phase: "result", Server: "k8s", OK: &ok,
		}},
		{Seq: 6, A2ATaskID: "run-1", Type: wire.EventStatus, Payload: wire.StatusPayload{
			State: wire.TaskCompleted,
		}},
	}
	ctx := context.Background()
	for _, ev := range events {
		require.NoError(t, sink.Event(ctx, ev), "event seq %d", ev.Seq)
	}

	// Every event forwarded VERBATIM to the inner run-events sink.
	require.Len(t, forwarded, len(events))

	// Scrape the registry exactly like a Prometheus scrape does.
	exposition := gatherExposition(t, reg)

	// The pipeline-liveness marker rides along (D3 degraded-state signal).
	require.Contains(t, exposition, "ksquad_tool_usage_pipeline_up")

	// The D3 read model (internal/apiserver) aggregates the same exposition.
	require.True(t, apiserver.ExpositionReportsToolUsage(exposition))
	agents, mcp := apiserver.AggregateToolUsageExposition(exposition)
	require.Len(t, agents, 1)
	require.Equal(t, "coder-1", agents[0].Agent)
	require.Len(t, agents[0].ToolCalls, 1)
	require.Equal(t, "shell", agents[0].ToolCalls[0].Tool)
	require.Equal(t, "git", agents[0].ToolCalls[0].Skill)
	require.Equal(t, uint64(1), agents[0].ToolCalls[0].Calls)
	require.Len(t, agents[0].SkillLoads, 1)
	require.Equal(t, uint64(1), agents[0].SkillLoads[0].Loads)
	require.Len(t, mcp, 1)
	require.Equal(t, "k8s", mcp[0].Server)
	require.Equal(t, uint64(1), mcp[0].Calls)
}

// TestTelemetrySinkUnknownOutcomeNeverPanics re-proves the tri-state contract
// through the production sink path: an orphan result (no start seen —
// at-least-once redelivery) maps to a synthesized complete span AND still
// counts; an unknown-phase event maps safely to a standalone span without a
// counter sample (result-based counting is the mapper's contract).
func TestTelemetrySinkUnknownOutcomeNeverPanics(t *testing.T) {
	reg := prometheus.NewRegistry()
	sink := NewTelemetrySink(DiscardSink, toolusage.NewMapper(nil, reg), toolusage.Labels{Agent: "x"})
	ctx := context.Background()
	require.NotPanics(t, func() {
		// Orphan result: counted, span synthesized.
		require.NoError(t, sink.Event(ctx, wire.Event{
			A2ATaskID: "run-2", Type: wire.EventTool,
			Payload: wire.ToolPayload{Name: "odd", Phase: "result"}, // OK absent → unknown outcome
		}))
		// Unknown phase: standalone span, never a panic, never dropped.
		require.NoError(t, sink.Event(ctx, wire.Event{
			A2ATaskID: "run-2", Type: wire.EventTool,
			Payload: wire.ToolPayload{Name: "weird", Phase: "???"},
		}))
	})
	exposition := gatherExposition(t, reg)
	agents, _ := apiserver.AggregateToolUsageExposition(exposition)
	require.Len(t, agents, 1)
	require.Equal(t, uint64(1), agents[0].ToolCalls[0].Calls) // orphan result counted once
}

// gatherExposition renders a registry to the Prometheus text format — the
// same bytes a scrape of /metrics would return.
func gatherExposition(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		require.NoError(t, enc.Encode(mf))
	}
	return buf.String()
}
