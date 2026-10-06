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
		{"qwen3.8:latest", 32768},                    // ISI-5113: local fleet model, was falling to 8192 default
		{"qwen3.6:latest", 32768},                    // longest-prefix "qwen3" wins
		{"qwen2.5-coder", 32768},                     // bare "qwen" prefix catches older families
		{"deepseek-flash", 65536},                    // ISI-5287: was falling to 8192 default → every run failed closed
		{"deepseek-v4-pro", 65536},                   // hosted DeepSeek API variant
		{"deepseek-r1:70b", 65536},                   // local Ollama variant
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

// TestWindowFor is the ISI-5540 per-endpoint resolution: an endpoint that
// DECLARES its window wins; absent a declaration it falls back to the
// conservative family catalog (so under-budget stays safe).
func TestWindowFor(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		declared int64
		want     int64
	}{
		// The required acceptance (task #4): a deepseek endpoint declaring 131072
		// earns the full 128K budget instead of the 64K family floor...
		{"declared 128K beats deepseek floor", "deepseek-chat", 131072, 131072},
		// ...and absent a declaration it falls back to 65536.
		{"no declaration falls back to family floor", "deepseek-chat", 0, 65536},
		{"local deepseek with no declaration stays conservative", "deepseek-r1:70b", 0, 65536},
		// A declaration is trusted as-is even below the family default (the
		// endpoint's own statement of what it serves — under-budget safe).
		{"smaller declaration is honored", "deepseek-chat", 32768, 32768},
		{"declaration wins for an otherwise-unknown model", "some-byo-model", 200000, 200000},
		{"no declaration, unknown model → fail-closed default", "some-byo-model", 0, DefaultContextWindow},
		{"negative declaration is ignored", "qwen3.8:latest", -1, 32768},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WindowFor(tc.model, tc.declared); got != tc.want {
				t.Errorf("WindowFor(%q, %d) = %d, want %d", tc.model, tc.declared, got, tc.want)
			}
		})
	}
}
