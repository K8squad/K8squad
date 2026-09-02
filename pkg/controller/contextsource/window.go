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

import "strings"

// The §10.1 Agent Card contextWindow is authored by the runtime shim
// post-dispatch (pkg/shim/runtimes/* stamp it on ModelInfo). The context
// assembler runs PRE-dispatch in the reconciler, so it needs a control-plane
// resolution of model → window without spawning the shim. This is that
// resolution: a small explicit catalog keyed by model family, mirroring the
// windows the conformant runtimes advertise, with a conservative default so a
// Run with an unrecognised model still assembles (the assembler only fails
// closed on a window <= 0, so a positive default keeps dispatch working —
// AC6 no-regression — while a known model gets its true window for AC5's
// budget math).
//
// This catalog is deliberately a code seam, not a CRD field: story S1 ships
// no new API surface. A first-class model→window resolution (Agent Card
// capability read, or an operator ModelCatalog) is the follow-up flagged on
// the S1 child issue.

// DefaultContextWindow is the conservative fallback for an unrecognised model.
const DefaultContextWindow int64 = 128000

// modelWindows maps a lower-cased model-id prefix to its context window
// (tokens). Longest-prefix match wins so "claude-sonnet-4-5" resolves off
// "claude-sonnet".
var modelWindows = []struct {
	prefix string
	window int64
}{
	{"claude-opus", 200000},
	{"claude-sonnet", 200000},
	{"claude-haiku", 200000},
	{"claude-3", 200000},
	{"claude-", 200000},
	{"gpt-4o", 128000},
	{"gpt-4.1", 1000000},
	{"gpt-4", 128000},
	{"o1", 200000},
	{"gemini-1.5", 1000000},
	{"gemini", 1000000},
}

// WindowForModel resolves a model id to its context window in tokens. An empty
// or unrecognised model resolves to DefaultContextWindow (never 0 — the
// assembler fails closed on a non-positive window, and an unknown model must
// not silently block dispatch).
func WindowForModel(model string) int64 {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return DefaultContextWindow
	}
	best := int64(0)
	bestLen := -1
	for _, e := range modelWindows {
		if strings.HasPrefix(m, e.prefix) && len(e.prefix) > bestLen {
			best = e.window
			bestLen = len(e.prefix)
		}
	}
	if bestLen < 0 {
		return DefaultContextWindow
	}
	return best
}
