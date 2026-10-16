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
	"strings"
	"testing"
)

// TestToolErrorSummary (ISI-5211): the tool-error normalizer collapses
// whitespace/newlines to single spaces, drops other control characters, trims,
// and caps the result — so a tool's failure reason reaches the run-feed chip
// as a bounded, PII-safe one-liner (NFR-2) rather than an unbounded blob.
func TestToolErrorSummary(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \n\t ", ""},
		{"plain reason", "invalid input syntax for type uuid", "invalid input syntax for type uuid"},
		{"collapses newlines", "line one\nline two\n\nline three", "line one line two line three"},
		{"collapses runs of spaces/tabs", "a\t\t  b", "a b"},
		{"trims edges", "  padded  ", "padded"},
		{"drops other control chars", "reason\x00\x07here", "reasonhere"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolErrorSummary(tc.in); got != tc.want {
				t.Fatalf("ToolErrorSummary(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A summary longer than the bound is capped with an explicit ellipsis so the
// wire (and the chip) can never be flooded by a runaway tool output.
func TestToolErrorSummaryBounds(t *testing.T) {
	long := strings.Repeat("x", ToolErrorSummaryMax+50)
	got := ToolErrorSummary(long)
	gotRunes := []rune(got)
	if len(gotRunes) != ToolErrorSummaryMax+1 { // capped runes + the ellipsis
		t.Fatalf("bounded length = %d runes, want %d", len(gotRunes), ToolErrorSummaryMax+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("bounded summary must end with an ellipsis, got %q", got[len(got)-4:])
	}
}
