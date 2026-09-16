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
	mu           sync.RWMutex
}

// NewTraceValidator creates a new trace validator
func NewTraceValidator() *TraceValidator {
	return &TraceValidator{
		expectedSpans: make(map[string]bool),
		actualSpans:   make(map[string]bool),
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
	
	missing := len(tv.GetMissingSpans())
	
	return fmt.Sprintf("Trace completeness: %d/%d spans (%.1f%% complete, %d missing)", 
		actual, expected, float64(actual)/float64(expected)*100, missing)
}

// TraceAnalyzer analyzes end-to-end traces for gaps
type TraceAnalyzer struct {
	validators map[string]*TraceValidator
	mu        sync.RWMutex
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

// AnalyzeTrace analyzes a trace for completeness
func (ta *TraceAnalyzer) AnalyzeTrace(ctx context.Context, span trace.Span) *TraceAnalysis {
	traceID := span.SpanContext().TraceID().String()
	
	validator := ta.GetTraceValidator(traceID)
	
	// Register the actual span
	spanName := span.Name()
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
		TraceID:     traceID,
		Validator:    validator,
		SpanName:     spanName,
		Timestamp:   time.Now(),
		IsComplete:  validator.IsComplete(),
		MissingSpans: validator.GetMissingSpans(),
	}
}

// TraceAnalysis represents the result of a trace analysis
type TraceAnalysis struct {
	TraceID     string
	Validator    *TraceValidator
	SpanName    string
	Timestamp   time.Time
	IsComplete  bool
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
		if now.Sub(validator.(*TraceValidator).Timestamp) > maxAge {
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
func AnalyzeSpan(ctx context.Context, span trace.Span) *TraceAnalysis {
	return GetGlobalAnalyzer().AnalyzeTrace(ctx, span)
}

// GenerateGlobalReport generates a report using the global analyzer
func GenerateGlobalReport(ctx context.Context) string {
	return GetGlobalAnalyzer().GenerateReport(ctx)
}