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

// DefaultContextWindow is the fail-closed fallback for an unrecognised model.
// It is deliberately SMALL (an Ollama-class local-model window, §8.5 budget
// doc) rather than a large hosted-model default: an unknown model must never
// be budgeted a 128K–1M prompt that its physical window cannot hold. A model
// with a genuinely larger window must be added to modelWindows (or resolved
// from its Agent Card) to earn it — under-budgeting is safe (the assembler
// fails closed on must-include overflow), over-budgeting silently breaks the
// runtime.
const DefaultContextWindow int64 = 8192

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
	// qwen3 backs the entire local bmad-squad/sympozium-squad fleet
	// (agent CR spec.model = "qwen3.8:latest", "qwen3.6:latest", …).
	// Before this entry every qwen agent fell to DefaultContextWindow (8192),
	// so the must-include tier (~8.3K tokens) overflowed an 8170-usable window
	// and EVERY run failed closed at context assembly (ISI-5113). 32768 is
	// qwen3's documented native window — a conservative, portable floor: the
	// live Ollama host (10.0.0.185) actually serves the full 262144, so this
	// under-budgets by design (§8.5: under-budget is safe, over-budget silently
	// breaks the runtime) while giving must-include 4× headroom. A precise
	// per-endpoint window belongs in the Agent Card / ModelEndpoint resolution
	// flagged above, not in this family-keyed catalog.
	{"qwen3", 32768},
	{"qwen", 32768},
	// deepseek-* (deepseek-flash, deepseek-v4-pro, deepseek-chat,
	// deepseek-reasoner, deepseek-r1, deepseek-v3) is reachable both via the
	// hosted DeepSeek API (api.deepseek.com) and the local Ollama host
	// (deepseek-r1:70b). Before this entry a deepseek model fell to
	// DefaultContextWindow (8192) so the must-include tier (~10K tokens)
	// overflowed and EVERY deepseek run failed closed at context assembly
	// (ISI-5287: john on deepseek-flash). 65536 is DeepSeek's documented
	// context window (64K) — a conservative, portable FLOOR giving must-include
	// ~6× headroom; the hosted API serves more for some variants (deepseek-chat
	// / V3 serve 128K). This under-budgets by design (§8.5: under-budget is safe,
	// over-budget silently breaks the runtime). The precise per-endpoint window
	// is resolved by WindowFor (ISI-5540): a BYO endpoint that DECLARES its true
	// window (modelendpoint contextWindow Secret key) earns it, while this entry
	// stays the floor for an endpoint that declares none — which is why it must
	// NOT be blanket-bumped to 128K (local Ollama deepseek-r1:70b may serve less,
	// and over-budget silently breaks the runtime).
	{"deepseek", 65536},
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

// WindowFor resolves a model's context window PER-ENDPOINT (ISI-5540). When the
// resolved ModelEndpoint DECLARES its own window (declared > 0 — the endpoint's
// 7.5 contextWindow Secret key, surfaced as modelendpoint.Endpoint.ContextWindow),
// that authoritative figure wins: the hosted DeepSeek API serves 128K for
// deepseek-chat/V3, so a BYO deepseek endpoint declaring 131072 is budgeted the
// full window instead of failing closed at the conservative 64K family floor.
//
// With NO declaration (declared <= 0) it falls back to the family-keyed catalog
// (WindowForModel) — the conservative default that keeps under-budget safe for
// an endpoint whose true window is unknown (e.g. a local Ollama deepseek-r1:70b
// that may serve less). The declared window is trusted as-is: it is the
// endpoint's own statement of what it serves, not a per-family guess, so an
// over-declaration is the endpoint operator's to own — never a blanket bump here.
func WindowFor(model string, declared int64) int64 {
	if declared > 0 {
		return declared
	}
	return WindowForModel(model)
}
