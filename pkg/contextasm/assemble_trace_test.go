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

package contextasm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// ============================================================================
// AC7 — trace instrumentation of the PUSH context-assembly path.
// (o11y contract: docs/observability/isi-3592-bootstrap-path-observability.md
//  §2 topology, §2.1/§2.2 attributes, §8 PII guardrail.)
//
// Each test pins one clause of AC7; deleting the instrumentation flips it.
// ============================================================================

// installTestTracer swaps the global TracerProvider for a synchronous in-memory
// recorder, so an Assemble pass's spans can be asserted without a collector. It
// restores the globals on cleanup. Mirrors rundrive/otel_test.go.
func installTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
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
	return exp
}

func spanByName(spans tracetest.SpanStubs, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

// AC7: a successful assembly emits exactly one `contextasm.assemble` span with
// the four `contextasm.source.*` child spans, and carries the §2.1 count/size
// attributes (authoritative element count proves the agent no longer starts on
// title+body alone — the exact G-gap this story closes).
func TestAssembleEmitsTraceTopology(t *testing.T) {
	exp := installTestTracer(t)
	src := fixtureSources()
	a := NewAssembler(src, 8)

	_, err := a.Assemble(context.Background(), fixtureReq(src, 200_000))
	require.NoError(t, err)

	spans := exp.GetSpans()
	root, ok := spanByName(spans, "contextasm.assemble")
	require.True(t, ok, "expected a contextasm.assemble span")

	// The four gather points each get a child span, parented to assemble.
	for _, name := range []string{
		"contextasm.source.work_item",
		"contextasm.source.project_meta",
		"contextasm.source.memory_recall",
		"contextasm.source.artifacts",
	} {
		child, ok := spanByName(spans, name)
		require.True(t, ok, "expected a %s span", name)
		assert.Equal(t, root.SpanContext.SpanID(), child.Parent.SpanID(),
			"%s must be a child of contextasm.assemble", name)
	}

	attrs := map[string]any{}
	for _, kv := range root.Attributes {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	// The SLO (bootstrap.no_silent_undercontext) requires >= 1 authoritative
	// element — the clause that guards the G-gap (agent must not start on
	// title+body alone). The fixture yields several (task + AC + goal + comment
	// + project-meta), so a concrete >1 also proves the count is populated.
	require.IsType(t, int64(0), attrs["ksquad.contextasm.elements.authoritative"])
	assert.Greater(t, attrs["ksquad.contextasm.elements.authoritative"].(int64), int64(1))
	assert.Equal(t, false, attrs["ksquad.contextasm.resume"])
	assert.Equal(t, int64(200_000), attrs["ksquad.contextasm.context_window"])
	assert.Equal(t, "wi-42", attrs["ksquad.run.work_item_ref"])
	// recall accounting present (returned from source, kept after budget).
	assert.Contains(t, attrs, "ksquad.contextasm.recall_docs.returned")
	assert.Contains(t, attrs, "ksquad.contextasm.recall_docs.kept")
	assert.Equal(t, codes.Unset, root.Status.Code, "success path leaves status unset")
}

// AC7 §8: the bootstrap path is the highest-PII surface — work-item text,
// comments and recall content must NEVER appear as a span attribute or event.
// This asserts the fixture's free-form bodies are absent from ALL telemetry.
func TestAssembleTraceEmitsNoContentPII(t *testing.T) {
	exp := installTestTracer(t)
	src := fixtureSources()
	a := NewAssembler(src, 8)

	_, err := a.Assemble(context.Background(), fixtureReq(src, 200_000))
	require.NoError(t, err)

	// Substrings unique to the free-form user text in the fixtures.
	forbidden := []string{
		"flakes on cold caches", // work-item description
		"Started on main",       // comment body
		"cache warmer races",    // recall content
		"e2e green 3x in a row", // acceptance criterion
	}
	for _, s := range exp.GetSpans() {
		var sb strings.Builder
		for _, kv := range s.Attributes {
			sb.WriteString(kv.Value.String())
			sb.WriteString("\x00")
		}
		for _, ev := range s.Events {
			for _, kv := range ev.Attributes {
				sb.WriteString(kv.Value.String())
				sb.WriteString("\x00")
			}
		}
		hay := sb.String()
		for _, bad := range forbidden {
			assert.NotContains(t, hay, bad,
				"span %q leaked content PII %q into telemetry", s.Name, bad)
		}
	}
}

// AC7: ErrMustIncludeExceedsWindow is the load-bearing fail-closed path — the
// span must carry error status AND ksquad.contextasm.fail_closed=true so the
// budget failure is never silent (spec §2.1/§7 bootstrap.fail_closed_visible).
func TestAssembleTraceMarksFailClosed(t *testing.T) {
	exp := installTestTracer(t)
	src := fixtureSources()
	src.wi.Description = strings.Repeat("word ", 9_000) // must-include > window
	a := NewAssembler(src, 8)

	_, err := a.Assemble(context.Background(), fixtureReq(src, 8_000))
	require.ErrorIs(t, err, ErrMustIncludeExceedsWindow)

	root, ok := spanByName(exp.GetSpans(), "contextasm.assemble")
	require.True(t, ok)
	assert.Equal(t, codes.Error, root.Status.Code, "fail-closed must set error status")

	var failClosed bool
	for _, kv := range root.Attributes {
		if string(kv.Key) == "ksquad.contextasm.fail_closed" {
			failClosed = kv.Value.AsBool()
		}
	}
	assert.True(t, failClosed, "ksquad.contextasm.fail_closed must be true on ErrMustIncludeExceedsWindow")
}
