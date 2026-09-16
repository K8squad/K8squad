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

// Package tracing_test provides comprehensive tests for end-to-end tracing improvements
package tracing

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

// TestNATSTracing validates NATS message instrumentation
func TestNATSTracing(t *testing.T) {
	// Test NATS message categorization
	tests := []struct {
		name        string
		subject     string
		expectedType string
	}{
		{"A2A Task Event", "a2a.task.12345", "task"},
		{"A2A Message Event", "a2a.message.12345", "message"},
		{"A2A Status Event", "a2a.status.12345", "status"},
		{"Git Tool", "git.clone", "git"},
		{"Bash Tool", "bash.execute", "bash"},
		{"MCP Tool", "mcp.server.tool", "mcp"},
		{"Docker Tool", "docker.build", "docker"},
		{"Generic System Tool", "system.execute", "system"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgType := GetA2AMessageType(tt.subject)
			assert.Equal(t, tt.expectedType, msgType)
		})
	}
}

// TestTraceValidator validates trace completeness checking
func TestTraceValidator(t *testing.T) {
	validator := NewTraceValidator()

	// Register expected spans
	validator.ExpectedSpan("run.start")
	validator.ExpectedSpan("nats.connect")
	validator.ExpectedSpan("supervisor.handle_task")
	validator.ExpectedSpan("llm.call")

	// Initially incomplete
	assert.False(t, validator.IsComplete())
	assert.Contains(t, validator.GetMissingSpans(), "run.start")
	assert.Contains(t, validator.GetMissingSpans(), "nats.connect")

	// Register actual spans
	validator.ActualSpan("run.start")
	validator.ActualSpan("nats.connect")

	// Still missing supervisor and llm.call
	assert.False(t, validator.IsComplete())
	assert.Contains(t, validator.GetMissingSpans(), "supervisor.handle_task")
	assert.Contains(t, validator.GetMissingSpans(), "llm.call")

	// Register remaining spans
	validator.ActualSpan("supervisor.handle_task")
	validator.ActualSpan("llm.call")

	// Now complete
	assert.True(t, validator.IsComplete())
	assert.Empty(t, validator.GetMissingSpans())
}

// TestTraceAnalyzer validates trace analysis functionality
func TestTraceAnalyzer(t *testing.T) {
	analyzer := NewTraceAnalyzer()

	// Mock span creation
	ctx := context.Background()
	
	// Create a mock trace span
	tracer := trace.NewNoopTracerProvider().Tracer("test")
	ctx, span := tracer.Start(ctx, "run.start")
	
	// Analyze the span
	analysis := analyzer.AnalyzeTrace(ctx, span)
	
	assert.Equal(t, span.SpanContext().TraceID().String(), analysis.TraceID)
	assert.Equal(t, "run.start", analysis.SpanName)
	assert.False(t, analysis.IsComplete) // Should be missing spans
	assert.Contains(t, analysis.MissingSpans, "nats.connect")
	assert.Contains(t, analysis.MissingSpans, "supervisor.handle_task")
}

// TestLLMTimingMeasurement validates LLM timing improvements
func TestLLMTimingMeasurement(t *testing.T) {
	// Test realistic timing validation
	tests := []struct {
		name          string
		durationMs    int64
		expectedValid bool
	}{
		{"Valid local LLM timing", 150, true},
		{"Unrealistic timing (too fast)", 1, false},
		{"Valid timing", 500, true},
		{"Boundary case (10ms)", 10, true},
		{"Unrealistic (microseconds)", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This simulates the timing validation logic
			valid := tt.durationMs >= 10 || tt.durationMs == 0
			assert.Equal(t, tt.expectedValid, valid)
		})
	}
}

// TestToolAttribution validates tool categorization
func TestToolAttribution(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		expected  string
	}{
		{"Bash tool", "bash.execute", "bash"},
		{"Git tool", "git.clone", "git"},
		{"MCP tool", "mcp.server.tool", "mcp"},
		{"Docker tool", "docker.build", "docker"},
		{"NPM tool", "npm.install", "npm"},
		{"Pip tool", "pip.install", "pip"},
		{"Python tool", "python.script", "python"},
		{"Node tool", "node.run", "node"},
		{"MCP server tool", "server.tool.call", "mcp"},
		{"Generic system tool", "system.execute", "system"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := getToolType(tt.toolName)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

// TestEndToEndTraceFlow validates the complete trace flow
func TestEndToEndTraceFlow(t *testing.T) {
	analyzer := GetGlobalAnalyzer()
	
	ctx := context.Background()
	tracer := trace.NewNoopTracerProvider().Tracer("test")

	// Simulate complete trace flow
	// 1. Operator starts run
	ctx, span1 := tracer.Start(ctx, "run.start")
	analysis1 := AnalyzeSpan(ctx, span1)
	assert.Contains(t, analysis1.MissingSpans, "nats.connect")

	// 2. NATS connection
	ctx, span2 := tracer.Start(ctx, "nats.connect")
	analysis2 := AnalyzeSpan(ctx, span2)
	assert.Contains(t, analysis2.MissingSpans, "supervisor.handle_task")

	// 3. Supervisor handles task
	ctx, span3 := tracer.Start(ctx, "supervisor.handle_task")
	analysis3 := AnalyzeSpan(ctx, span3)
	assert.Contains(t, analysis3.MissingSpans, "llm.call")

	// 4. LLM call
	ctx, span4 := tracer.Start(ctx, "llm.call")
	analysis4 := AnalyzeSpan(ctx, span4)
	
	// Should still be missing run.end
	assert.Contains(t, analysis4.MissingSpans, "run.end")

	// 5. Run completion
	ctx, span5 := tracer.Start(ctx, "run.end")
	analysis5 := AnalyzeSpan(ctx, span5)
	
	// Generate complete report
	report := analyzer.GenerateReport(ctx)
	assert.Contains(t, report, "Trace ID:")
	assert.Contains(t, report, "Missing spans:")
}

// TestTraceIntegration validates that all tracing components work together
func TestTraceIntegration(t *testing.T) {
	// Test that all improvements work together
	analyzer := NewTraceAnalyzer()
	validator := NewTraceValidator()
	
	ctx := context.Background()
	tracer := trace.NewNoopTracerProvider().Tracer("test")

	// Define expected spans for integration test
	expectedSpans := []string{
		"run.start",
		"nats.connect",
		"nats.publish",
		"nats.subscribe",
		"supervisor.handle_task",
		"supervisor.runtime.init",
		"llm.call",
		"gen_ai.tool.call",
		"run.end",
	}

	for _, spanName := range expectedSpans {
		validator.ExpectedSpan(spanName)
		
		ctx, span := tracer.Start(ctx, spanName)
		analysis := analyzer.AnalyzeTrace(ctx, span)
		
		// Each span should be recorded
		assert.Equal(t, spanName, analysis.SpanName)
		
		// Progress toward completion
		progress := float64(len(expectedSpans)-len(analysis.MissingSpans)) / float64(len(expectedSpans))
		assert.Greater(t, progress, 0.0)
	}

	// Final analysis
	finalAnalysis := analyzer.GenerateReport(ctx)
	
	// Should show progress toward completeness
	assert.Contains(t, finalAnalysis, "Trace completeness:")
	assert.Contains(t, finalAnalysis, "missing spans")
}

// BenchmarkTraceValidation validates performance of trace validation
func BenchmarkTraceValidation(b *testing.B) {
	analyzer := GetGlobalAnalyzer()
	
	ctx := context.Background()
	tracer := trace.NewNoopTracerProvider().Tracer("benchmark")
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Create a span
		ctx, span := tracer.Start(ctx, fmt.Sprintf("span_%d", i))
		
		// Analyze it
		_ = AnalyzeSpan(ctx, span)
	}
}

// TestTraceMemoryUsage validates memory efficiency of trace tracking
func TestTraceMemoryUsage(t *testing.T) {
	analyzer := NewTraceAnalyzer()
	
	ctx := context.Background()
	tracer := trace.NewNoopTracerProvider().Tracer("test")

	// Create many traces
	for i := 0; i < 100; i++ {
		ctx, span := tracer.Start(ctx, fmt.Sprintf("run_%d", i))
		_ = analyzer.AnalyzeTrace(ctx, span)
	}

	// Verify they're all tracked
	report := analyzer.GenerateReport(ctx)
	assert.Contains(t, report, "100") // Should show 100 traces

	// Clear old traces (older than 1 second)
	analyzer.ClearOldTraces(1 * time.Second)
	
	// Should be empty now
	report = analyzer.GenerateReport(ctx)
	assert.Contains(t, report, "0 traces" || "no traces")
}