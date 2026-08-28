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

package toolusage_test

// The D1 AC2 end-to-end proof (plan §5 item 4): tool/skill/MCP activity fed
// through the Mapper arrives at a REAL OTLP/HTTP collector as GenAI-semconv
// spans — not as in-memory SDK spans, but as decoded OTLP protobuf on the
// wire, which is what an operator's collector would receive. The collector is
// an in-process httptest.Server speaking /v1/traces protobuf; the exporter is
// the production otlptracehttp client.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

// otlpCollector is the in-process OTLP/HTTP test collector: it accepts
// ExportTraceServiceRequest protobuf bodies on /v1/traces and retains every
// scope span for assertion.
type otlpCollector struct {
	srv *httptest.Server

	mu    sync.Mutex
	spans []*otlptrace.Span
}

func newOTLPCollector(t *testing.T) *otlpCollector {
	t.Helper()
	c := &otlpCollector{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
			return
		}
		var req collectortrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, "unmarshal: "+err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				c.spans = append(c.spans, ss.Spans...)
			}
		}
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{})
	})
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

// flushed drives the exporter synchronously: one UploadSpan is forced by
// shutdown, so every span emitted before this call has crossed the wire.
func (c *otlpCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.spans)
}

// attrs flattens an OTLP span's attributes to string→string (every attribute
// this package emits is string-typed).
func attrs(s *otlptrace.Span) map[string]string {
	m := map[string]string{}
	for _, kv := range s.Attributes {
		if kv.Value != nil && kv.Value.GetStringValue() != "" {
			m[kv.Key] = kv.Value.GetStringValue()
		}
	}
	return m
}

func findSpan(t *testing.T, spans []*otlptrace.Span, name string) *otlptrace.Span {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no span named %q among %d exported", name, len(spans))
	return nil
}

// TestOTLPExportGenAISemconvSpans is D1 AC2: a real OTLP/HTTP hop carries
// gen_ai.tool.call, skill.load and mcp.call spans with their GenAI-semconv and
// ksquad.* attributes intact, hashed (never raw) arguments, and mapped
// outcomes.
func TestOTLPExportGenAISemconvSpans(t *testing.T) {
	collector := newOTLPCollector(t)
	ctx := context.Background()

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(strings.TrimPrefix(collector.srv.URL, "http://")),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		t.Fatalf("otlp exporter: %v", err)
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless())
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithResource(res),
	)
	defer func() { _ = tp.Shutdown(ctx) }()

	toolusage.SetEnabled(true)
	m := toolusage.NewMapper(tp.Tracer("ksquad-toolusage-e2e"), nil)
	labels := toolusage.Labels{RunID: "run-e2e-1", Agent: "coder"}

	ok, fail := true, false
	m.ToolEvent(ctx, labels, "task-1", a2a.ToolPayload{
		Name: "shell", Phase: "start", ArgsSHA256: "deadbeef", Skill: "git",
	})
	m.ToolEvent(ctx, labels, "task-1", a2a.ToolPayload{
		Name: "shell", Phase: "result", OK: &ok,
	})
	m.ToolEvent(ctx, labels, "task-2", a2a.ToolPayload{
		Name: "k8s.get", Phase: "start", Server: "k8s-mcp",
	})
	m.ToolEvent(ctx, labels, "task-2", a2a.ToolPayload{
		Name: "k8s.get", Phase: "result", OK: &fail, Server: "k8s-mcp",
	})
	m.SkillEvent(ctx, labels, a2a.SkillLoadPayload{
		Name: "code-review", SHA256: "cafebabe", OK: &ok,
	})

	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown flush: %v", err)
	}

	collector.mu.Lock()
	spans := append([]*otlptrace.Span(nil), collector.spans...)
	collector.mu.Unlock()
	if len(spans) != 3 {
		t.Fatalf("want 3 exported spans (tool, mcp, skill), got %d", len(spans))
	}

	tool := findSpan(t, spans, "gen_ai.tool.call")
	ta := attrs(tool)
	if want := "shell"; ta["gen_ai.tool.name"] != want {
		t.Errorf("gen_ai.tool.name = %q, want %q", ta["gen_ai.tool.name"], want)
	}
	if want := "deadbeef"; ta["gen_ai.tool.call.arguments"] != want {
		t.Errorf("gen_ai.tool.call.arguments = %q, want the sha256 %q (raw args never travel)", ta["gen_ai.tool.call.arguments"], want)
	}
	if ta["ksquad.outcome"] != "success" || ta["ksquad.run.id"] != "run-e2e-1" || ta["ksquad.agent.name"] != "coder" {
		t.Errorf("tool span correlation/outcome attrs = %v", ta)
	}
	if ta["ksquad.skill.name"] != "git" {
		t.Errorf("ksquad.skill.name = %q, want git", ta["ksquad.skill.name"])
	}
	if tool.Status == nil || tool.Status.Code != otlptrace.Status_STATUS_CODE_OK {
		t.Errorf("tool span status = %+v, want OK", tool.Status)
	}

	mcp := findSpan(t, spans, "mcp.call")
	ma := attrs(mcp)
	if ma["gen_ai.tool.name"] != "k8s.get" || ma["ksquad.mcp.server"] != "k8s-mcp" {
		t.Errorf("mcp span attrs = %v", ma)
	}
	if ma["ksquad.outcome"] != "error" {
		t.Errorf("mcp outcome = %q, want error", ma["ksquad.outcome"])
	}
	if mcp.Status == nil || mcp.Status.Code != otlptrace.Status_STATUS_CODE_ERROR {
		t.Errorf("mcp span status = %+v, want ERROR", mcp.Status)
	}

	skill := findSpan(t, spans, "skill.load")
	sa := attrs(skill)
	if sa["ksquad.skill.name"] != "code-review" || sa["ksquad.skill.source.sha"] != "cafebabe" {
		t.Errorf("skill span attrs = %v", sa)
	}
	if skill.Status == nil || skill.Status.Code != otlptrace.Status_STATUS_CODE_OK {
		t.Errorf("skill span status = %+v, want OK", skill.Status)
	}
}

// TestOTLPExportGateOff proves the D2 gate end-to-end: SetEnabled(false)
// silences the pipeline before the wire — nothing reaches the collector.
func TestOTLPExportGateOff(t *testing.T) {
	collector := newOTLPCollector(t)
	ctx := context.Background()

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(strings.TrimPrefix(collector.srv.URL, "http://")),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		t.Fatalf("otlp exporter: %v", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(ctx) }()

	toolusage.SetEnabled(false)
	t.Cleanup(func() { toolusage.SetEnabled(true) })
	m := toolusage.NewMapper(tp.Tracer("ksquad-toolusage-e2e"), nil)

	ok := true
	m.ToolEvent(ctx, toolusage.Labels{RunID: "r", Agent: "a"}, "t", a2a.ToolPayload{
		Name: "shell", Phase: "start",
	})
	m.ToolEvent(ctx, toolusage.Labels{RunID: "r", Agent: "a"}, "t", a2a.ToolPayload{
		Name: "shell", Phase: "result", OK: &ok,
	})
	m.SkillEvent(ctx, toolusage.Labels{RunID: "r", Agent: "a"}, a2a.SkillLoadPayload{
		Name: "code-review", OK: &ok,
	})

	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown flush: %v", err)
	}
	if n := collector.count(); n != 0 {
		t.Fatalf("gate off: want 0 exported spans, got %d", n)
	}
}
