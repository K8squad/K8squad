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

package tracing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/trace"
)

// TestTraceValidator validates trace completeness checking
func TestTraceValidator(t *testing.T) {
	validator := NewTraceValidator()

	validator.ExpectedSpan("run.start")
	validator.ExpectedSpan("nats.publish")
	validator.ExpectedSpan("supervisor.handle_task")
	validator.ExpectedSpan("llm.call")

	assert.False(t, validator.IsComplete())
	assert.Contains(t, validator.GetMissingSpans(), "run.start")
	assert.Contains(t, validator.GetMissingSpans(), "nats.publish")

	validator.ActualSpan("run.start")
	validator.ActualSpan("nats.publish")

	assert.False(t, validator.IsComplete())
	assert.Contains(t, validator.GetMissingSpans(), "supervisor.handle_task")
	assert.Contains(t, validator.GetMissingSpans(), "llm.call")

	validator.ActualSpan("supervisor.handle_task")
	validator.ActualSpan("llm.call")

	assert.True(t, validator.IsComplete())
	assert.Empty(t, validator.GetMissingSpans())
}

// TestTraceAnalyzer validates trace analysis functionality
func TestTraceAnalyzer(t *testing.T) {
	analyzer := NewTraceAnalyzer()

	ctx := context.Background()
	tracer := trace.NewNoopTracerProvider().Tracer("test")
	_, span := tracer.Start(ctx, "run.start")

	analysis := analyzer.AnalyzeTrace(ctx, "run.start", span)

	assert.Equal(t, "run.start", analysis.SpanName)
	assert.False(t, analysis.IsComplete)
	assert.Contains(t, analysis.MissingSpans, "nats.publish")
	assert.Contains(t, analysis.MissingSpans, "supervisor.handle_task")
}

// TestAnalyzeSpanGlobal exercises the global-analyzer convenience wrapper.
func TestAnalyzeSpanGlobal(t *testing.T) {
	ctx := context.Background()
	tracer := trace.NewNoopTracerProvider().Tracer("test")
	_, span := tracer.Start(ctx, "llm.call")

	analysis := AnalyzeSpan(ctx, "llm.call", span)
	assert.Equal(t, "llm.call", analysis.SpanName)
}

// TestClearOldTraces validates age-based validator eviction
func TestClearOldTraces(t *testing.T) {
	analyzer := NewTraceAnalyzer()

	ctx := context.Background()
	tracer := trace.NewNoopTracerProvider().Tracer("test")
	_, span := tracer.Start(ctx, "run.start")
	_ = analyzer.AnalyzeTrace(ctx, "run.start", span)

	// Fresh traces survive a non-zero maxAge
	analyzer.ClearOldTraces(time.Hour)
	report := analyzer.GenerateReport(ctx)
	assert.Contains(t, report, "Trace ID:")

	// Zero maxAge evicts everything
	analyzer.ClearOldTraces(0)
	report = analyzer.GenerateReport(ctx)
	assert.NotContains(t, report, "Trace ID:")
}
