/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the limitations under the License.
*/

package a2a

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	collectortracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

func boolPtrSink(b bool) *bool { return &b }

// collector is a minimal OTLP/HTTP trace test-collector (D1 AC2): it accepts
// protobuf export requests and keeps every received span, queryable by name
// and attribute.
type collector struct {
	srv *httptest.Server
	mu  sync.Mutex
	// spans is flat: {spanName → {attrKey → attrValue}}
	spans []map[string]string
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req collectortracev1.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		for _, rs := range req.ResourceSpans {
			for _, sc := range rs.ScopeSpans {
				for _, sp := range sc.Spans {
					flat := map[string]string{"name": sp.Name}
					for _, a := range sp.Attributes {
						flat[a.Key] = a.Value.GetStringValue()
					}
					c.spans = append(c.spans, flat)
				}
			}
		}
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) query(name, key, val string) []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]string
	for _, s := range c.spans {
		if s["name"] != name {
			continue
		}
		if key != "" && s[key] != val {
			continue
		}
		out = append(out, s)
	}
	return out
}

// TestTelemetrySinkSpansReachOTLPCollector is D1 AC2: an A2A event stream
// through the TelemetrySink lands as GenAI-semconv spans at an OTLP
// collector, queryable per run and per agent.
func TestTelemetrySinkSpansReachOTLPCollector(t *testing.T) {
	col := newCollector(t)
	exp, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpoint(col.srv.Listener.Addr().String()),
		otlptracehttp.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}

	// Real pipeline: exporter → mapper → sink, over the A2A event shapes.
	mapper := newSinkMapper(t, exp)
	labels := toolusage.Labels{RunID: "11111111-2222-3333-4444-555555555555", Agent: "backup-coder"}
	sink := NewTelemetrySink(DiscardSink, mapper, labels)
	task := "11111111-2222-3333-4444-555555555555"

	events := []wire.Event{
		{Seq: 1, A2ATaskID: task, Type: wire.EventTool, Payload: wire.ToolPayload{
			Name: "kubectl", Phase: "start", ArgsSHA256: "ab12", Skill: "restart-deploy",
		}},
		{Seq: 2, A2ATaskID: task, Type: wire.EventTool, Payload: map[string]any{ // stdio form: generic JSON
			"name": "create_pull_request", "phase": "start", "server": "github-mcp",
		}},
		{Seq: 3, A2ATaskID: task, Type: wire.EventTool, Payload: wire.ToolPayload{
			Name: "kubectl", Phase: "result", OK: boolPtrSink(true),
		}},
		{Seq: 4, A2ATaskID: task, Type: wire.EventTool, Payload: map[string]any{
			"name": "create_pull_request", "phase": "result", "ok": true, "server": "github-mcp",
		}},
		{Seq: 5, A2ATaskID: task, Type: wire.EventSkillLoad, Payload: map[string]any{
			"name": "restart-deploy", "sha256": "cafe1", "ok": true,
		}},
		{Seq: 6, A2ATaskID: task, Type: wire.EventStatus, Payload: map[string]any{"state": "completed"}},
	}
	ctx := context.Background()
	for _, ev := range events {
		if err := sink.Event(ctx, ev); err != nil {
			t.Fatalf("event seq %d: %v", ev.Seq, err)
		}
	}

	// The batch exporter flushes asynchronously — poll the collector with a
	// bounded wait (D1 AC2 asserts presence, not latency).
	deadline := time.Now().Add(5 * time.Second)
	for len(col.query("skill.load", "", "")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no spans reached the collector within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := col.query(toolusage.SpanToolCall, "ksquad.run.id", labels.RunID); len(got) != 1 {
		t.Errorf("gen_ai.tool.call queryable per run: got %d spans, want 1", len(got))
	}
	if got := col.query(toolusage.SpanToolCall, "gen_ai.tool.name", "kubectl"); len(got) != 1 {
		t.Errorf("gen_ai.tool.call queryable per tool: got %d, want 1", len(got))
	}
	if got := col.query(toolusage.SpanToolCall, "ksquad.agent.name", labels.Agent); len(got) != 1 {
		t.Errorf("gen_ai.tool.call queryable per agent: got %d, want 1", len(got))
	}
	mcp := col.query(toolusage.SpanMCPCall, "ksquad.mcp.server", "github-mcp")
	if len(mcp) != 1 || mcp[0]["gen_ai.tool.name"] != "create_pull_request" {
		t.Errorf("mcp.call span wrong: %+v", mcp)
	}
	sk := col.query(toolusage.SpanSkillLoad, "ksquad.skill.source.sha", "cafe1")
	if len(sk) != 1 || sk[0]["ksquad.skill.name"] != "restart-deploy" {
		t.Errorf("skill.load span wrong: %+v", sk)
	}
}

// TestTelemetrySinkForwardVerbatim: the decorated sink receives every event
// unchanged — telemetry never mutates or drops the stream.
func TestTelemetrySinkForwardVerbatim(t *testing.T) {
	var got []wire.Event
	inner := SinkFunc(func(_ context.Context, ev wire.Event) error {
		got = append(got, ev)
		return nil
	})
	sink := NewTelemetrySink(inner, nil, toolusage.Labels{}) // nil mapper: pure pass-through

	in := []wire.Event{
		{Seq: 1, A2ATaskID: "t", Type: wire.EventTool, Payload: wire.ToolPayload{Name: "x", Phase: "start"}},
		{Seq: 2, A2ATaskID: "t", Type: wire.EventMessage, Payload: wire.MessagePayload{Role: "agent", Text: "hi", Trust: "untrusted"}},
	}
	for _, ev := range in {
		if err := sink.Event(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != len(in) {
		t.Fatalf("forwarded %d events, want %d", len(got), len(in))
	}
	if got[0].Seq != 1 || got[1].Type != wire.EventMessage {
		t.Errorf("forwarded events mutated: %+v", got)
	}
}

// TestTelemetrySinkInnerErrorAborts keeps the §5.1 EventSink contract: an
// inner sink error surfaces from the wrapper (telemetry must swallow its
// own, but never mask the decorated sink's).
func TestTelemetrySinkInnerErrorAborts(t *testing.T) {
	boom := SinkFunc(func(context.Context, wire.Event) error { return errSink })
	mapper := newSinkMapper(t, nil)
	sink := NewTelemetrySink(boom, mapper, toolusage.Labels{Agent: "a"})
	if err := sink.Event(context.Background(), wire.Event{
		A2ATaskID: "t", Type: wire.EventTool, Payload: wire.ToolPayload{Name: "x", Phase: "start"},
	}); err == nil {
		t.Fatal("inner error must propagate")
	}
}

type sinkErr struct{}

func (sinkErr) Error() string { return "sink rejected" }

var errSink error = sinkErr{}

var _ attribute.KeyValue // attribute import guard for future flat-attr assertions
