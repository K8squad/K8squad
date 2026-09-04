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

package contextsource

import "testing"

func TestWindowForModel(t *testing.T) {
	cases := []struct {
		model string
		want  int64
	}{
		{"claude-sonnet-4", 200000},
		{"claude-sonnet-4-5-20260101", 200000},
		{"claude-opus-4-8", 200000},
		{"gpt-4o", 128000},
		{"gpt-4.1", 1000000},
		{"gemini-1.5-pro", 1000000},
		{"", DefaultContextWindow},                   // no model → default
		{"some-unknown-model", DefaultContextWindow}, // unknown → default, never 0
	}
	for _, tc := range cases {
		if got := WindowForModel(tc.model); got != tc.want {
			t.Errorf("WindowForModel(%q) = %d, want %d", tc.model, got, tc.want)
		}
	}
	// The assembler fails closed on window <= 0; every resolution must be positive.
	if WindowForModel("literally-anything") <= 0 {
		t.Error("WindowForModel returned a non-positive window for an unknown model")
	}
}
