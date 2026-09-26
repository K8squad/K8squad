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

// Package tracing validates and analyzes end-to-end trace completeness
package tracing

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// TraceValidator validates end-to-end trace completeness
type TraceValidator struct {
	expectedSpans map[string]bool
	actualSpans   map[string]bool
	// ISI-5012 (P1#3): flow-level completeness — exactly one root span per
	// trace (request.is_root_span=true) and a non-empty status on every
	// llm.call span. These are recorded by the caller (the OTel Span API
	// does not expose attributes/status, so the observer supplies them, same
	// as the span name).
	rootSpans        int
	llmCalls         int
	llmCallsNoStatus int
	createdAt        time.Time
	mu               sync.RWMutex
}

// NewTraceValidator creates a new trace validator
func NewTraceValidator() *TraceValidator {
	return &TraceValidator{
		expectedSpans: make(map[string]bool),
		actualSpans:   make(map[string]bool),
		createdAt:     time.Now(),
	}
}

// ExpectedSpan registers an expected span type
func (tv *TraceValidator) ExpectedSpan(spanType string) {
	tv.mu.Lock()
	defer tv.mu.Unlock()
	tv.expectedSpans[spanType] = true
}

// ActualSpan registers an actual span that was observed
func (tv *TraceValidator) ActualSpan(spanType string) {
	tv.mu.Lock()
	defer tv.mu.Unlock()
	tv.actualSpans[spanType] = true
}

// RecordRootSpan registers that a span was observed carrying
// request.is_root_span=true (ISI-5012). Each flow must mark exactly one.
func (tv *TraceValidator) RecordRootSpan() {
	tv.mu.Lock()
	defer tv.mu.Unlock()
	tv.rootSpans++
}

// RecordLLMCall registers an observed llm.call span and whether it carried a
// non-empty span.status_code (ISI-5012); statusSet=false records a gap.
func (tv *TraceValidator) RecordLLMCall(statusSet bool) {
	tv.mu.Lock()
	defer tv.mu.Unlock()
	tv.llmCalls++
	if !statusSet {
		tv.llmCallsNoStatus++
	}
}

// RootSpanCount returns how many spans were recorded as the flow root.
func (tv *TraceValidator) RootSpanCount() int {
	tv.mu.RLock()
	defer tv.mu.RUnlock()
	return tv.rootSpans
}

// LLMCallCount returns how many llm.call spans were observed.
func (tv *TraceValidator) LLMCallCount() int {
	tv.mu.RLock()
	defer tv.mu.RUnlock()
	return tv.llmCalls
}

// LLMCallsWithoutStatus returns how many llm.call spans lacked a status code.
func (tv *TraceValidator) LLMCallsWithoutStatus() int {
	tv.mu.RLock()
	defer tv.mu.RUnlock()
	return tv.llmCallsNoStatus
}

// HasSingleRoot reports whether exactly one root span was observed.
func (tv *TraceValidator) HasSingleRoot() bool { return tv.RootSpanCount() == 1 }

// AllLLMCallsHaveStatus reports whether at least one llm.call span was observed
// and every one carried a non-empty status.
func (tv *TraceValidator) AllLLMCallsHaveStatus() bool {
	return tv.LLMCallCount() > 0 && tv.LLMCallsWithoutStatus() == 0
}

// GetMissingSpans returns the set of expected spans that were not observed
func (tv *TraceValidator) GetMissingSpans() []string {
	tv.mu.RLock()
	defer tv.mu.RUnlock()

	var missing []string
	for span := range tv.expectedSpans {
		if !tv.actualSpans[span] {
			missing = append(missing, span)
		}
	}
	return missing
}

// IsComplete returns whether all expected spans were observed
func (tv *TraceValidator) IsComplete() bool {
	tv.mu.RLock()
	defer tv.mu.RUnlock()

	for span := range tv.expectedSpans {
		if !tv.actualSpans[span] {
			return false
		}
	}
	return true
}

// GetSummary returns a completeness summary
func (tv *TraceValidator) GetSummary() string {
	tv.mu.RLock()
	defer tv.mu.RUnlock()

	expected := len(tv.expectedSpans)
	actual := 0
	for span := range tv.expectedSpans {
		if tv.actualSpans[span] {
			actual++
		}
	}

	missing := expected - actual

	return fmt.Sprintf("Trace completeness: %d/%d spans (%.1f%% complete, %d missing)",
		actual, expected, float64(actual)/float64(expected)*100, missing)
}

// TraceAnalyzer analyzes end-to-end traces for gaps
type TraceAnalyzer struct {
	validators map[string]*TraceValidator
	mu         sync.RWMutex
}

// NewTraceAnalyzer creates a new trace analyzer
func NewTraceAnalyzer() *TraceAnalyzer {
	return &TraceAnalyzer{
		validators: make(map[string]*TraceValidator),
	}
}

// GetTraceValidator gets or creates a validator for a specific trace ID
func (ta *TraceAnalyzer) GetTraceValidator(traceID string) *TraceValidator {
	ta.mu.Lock()
	defer ta.mu.Unlock()

	if validator, exists := ta.validators[traceID]; exists {
		return validator
	}

	validator := NewTraceValidator()
	ta.validators[traceID] = validator
	return validator
}

// AnalyzeTrace analyzes a trace for completeness. The OTel Span API does not
// expose a span's name, so the caller supplies it (the name the span was
// started with).
func (ta *TraceAnalyzer) AnalyzeTrace(ctx context.Context, spanName string, span trace.Span) *TraceAnalysis {
	traceID := span.SpanContext().TraceID().String()

	validator := ta.GetTraceValidator(traceID)

	// Register the actual span
	validator.ActualSpan(spanName)

	// Define expected spans for this trace type
	if spanName == "run.start" {
		validator.ExpectedSpan("run.start")
		validator.ExpectedSpan("nats.connect")
		validator.ExpectedSpan("nats.publish")
		validator.ExpectedSpan("nats.subscribe")
		validator.ExpectedSpan("supervisor.handle_task")
		validator.ExpectedSpan("supervisor.runtime.init")
		validator.ExpectedSpan("llm.call")
		validator.ExpectedSpan("gen_ai.tool.call")
		validator.ExpectedSpan("run.end")
	}

	return &TraceAnalysis{
		TraceID:      traceID,
		Validator:    validator,
		SpanName:     spanName,
		Timestamp:    time.Now(),
		IsComplete:   validator.IsComplete(),
		MissingSpans: validator.GetMissingSpans(),
	}
}

// TraceAnalysis represents the result of a trace analysis
type TraceAnalysis struct {
	TraceID      string
	Validator    *TraceValidator
	SpanName     string
	Timestamp    time.Time
	IsComplete   bool
	MissingSpans []string
}

// GenerateReport generates a detailed trace report
func (ta *TraceAnalyzer) GenerateReport(ctx context.Context) string {
	ta.mu.RLock()
	defer ta.mu.RUnlock()

	report := "=== End-to-End Trace Analysis Report ===\n\n"

	for traceID, validator := range ta.validators {
		report += fmt.Sprintf("Trace ID: %s\n", traceID)
		report += fmt.Sprintf("  %s\n", validator.GetSummary())

		if len(validator.GetMissingSpans()) > 0 {
			report += "  Missing spans:\n"
			for _, missing := range validator.GetMissingSpans() {
				report += fmt.Sprintf("    - %s\n", missing)
			}
		} else {
			report += "  All expected spans found ✓\n"
		}
		report += "\n"
	}

	return report
}

// ClearOldTraces clears validators for traces older than the specified duration
func (ta *TraceAnalyzer) ClearOldTraces(maxAge time.Duration) {
	ta.mu.Lock()
	defer ta.mu.Unlock()

	now := time.Now()
	for traceID, validator := range ta.validators {
		if now.Sub(validator.createdAt) > maxAge {
			delete(ta.validators, traceID)
		}
	}
}

// GetGlobalAnalyzer returns a global trace analyzer instance
var (
	globalAnalyzer *TraceAnalyzer
	analyzerOnce   sync.Once
)

func GetGlobalAnalyzer() *TraceAnalyzer {
	analyzerOnce.Do(func() {
		globalAnalyzer = NewTraceAnalyzer()
	})
	return globalAnalyzer
}

// AnalyzeSpan analyzes a span using the global analyzer
func AnalyzeSpan(ctx context.Context, spanName string, span trace.Span) *TraceAnalysis {
	return GetGlobalAnalyzer().AnalyzeTrace(ctx, spanName, span)
}

// GenerateGlobalReport generates a report using the global analyzer
func GenerateGlobalReport(ctx context.Context) string {
	return GetGlobalAnalyzer().GenerateReport(ctx)
}
