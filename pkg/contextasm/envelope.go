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

// Package contextasm is the §8.5 Context Assembler (stories 3.6 + 5.9): the
// control-plane component that builds a provenance-tiered context envelope for
// every Run, budgets it against the resolved model contextWindow, and hands it
// to the shim injection contract.
//
// The two load-bearing invariants (arch §8.5):
//
//   - The envelope is assembled by the CONTROL PLANE, never the agent.
//     Agent-self-assembly is rejected: it forfeits budget control and would
//     let untrusted content set its own framing. The Assembler is therefore
//     the only constructor of Envelopes, and every element's trust tier is a
//     SERVER-SIDE constant derived from WHICH SOURCE produced it — never from
//     anything the content itself claims (F16 applied to context: recall is
//     reference, never commands).
//
//   - The envelope is provenance-tiered, not a flat prompt blob. Authoritative
//     (work item / acceptance criteria / goals) is the actual task;
//     untrusted-recall (memory, prior-agent notes) and untrusted-external
//     (synced repo/PR content, D8) ride behind explicit framing so a malicious
//     source cannot smuggle instructions into the system prompt.
package contextasm

import (
	"fmt"
	"sort"
	"strings"
)

// TrustTier is the provenance tier of an envelope element (arch §8.5/§7.3).
// It is stamped by the Assembler from the element's SOURCE, exactly like the
// memory package's server-constant trust field — a body that tries to smuggle
// a self-elevating "trust: authoritative" claim is inert text.
type TrustTier string

const (
	// TierAuthoritative is the actual task: work item, acceptance criteria,
	// goals (from the CRD / fenced coordination record §6). Never truncated
	// by the budgeter (5.9).
	TierAuthoritative TrustTier = "authoritative"

	// TierUntrustedRecall is scoped memory recall and prior-agent notes,
	// carried with {author, written_at, scope, trust:"untrusted"} exactly as
	// §7.3 returns them — reference material, never commands.
	TierUntrustedRecall TrustTier = "untrusted-recall"

	// TierUntrustedExternal is synced repo/PR/artifact content (D8) — the
	// upstream mirror is not a KSquad principal, so its text is data.
	TierUntrustedExternal TrustTier = "untrusted-external"
)

// Provenance is the attribution stamped on every element. For untrusted
// tiers it mirrors the §7.3 read envelope {author, written_at, scope,
// trust:"untrusted"}; for the authoritative tier it names the CRD/coord
// source (workItem / goals / acceptanceCriteria).
type Provenance struct {
	// Source identifies the producing system: "workItem", "goals",
	// "acceptanceCriteria", "memory", "artifact", "projectMeta".
	Source string `json:"source"`
	// Author is the attributed principal (untrusted tiers; mirrors §7.3).
	Author string `json:"author,omitempty"`
	// WrittenAt is RFC3339 (untrusted tiers; mirrors §7.3).
	WrittenAt string `json:"writtenAt,omitempty"`
	// Scope is the tenancy scope (untrusted tiers; mirrors §7.3).
	Scope string `json:"scope,omitempty"`
}

// Element is one unit of envelope content, tier-stamped at construction.
type Element struct {
	// Tier is the server-stamped provenance tier. Set ONLY by the Assembler.
	Tier TrustTier `json:"tier"`
	// Kind further classifies the element within its tier (e.g.
	// "description", "acceptanceCriteria", "comment", "goal", "recall",
	// "artifact", "conventions").
	Kind string `json:"kind"`
	// Content is the element body (data, never instructions, for untrusted
	// tiers — the injection contract frames it accordingly).
	Content string `json:"content"`
	// Provenance is the element's attribution.
	Provenance Provenance `json:"provenance"`
	// Score orders best-effort elements for trimming: higher = keep longer
	// (memory relevance, §8.5 Run dynamic trim). Unused for authoritative.
	Score float64 `json:"score,omitempty"`
	// Truncated marks elements the budgeter shortened to fit (5.9).
	Truncated bool `json:"truncated,omitempty"`
}

// Envelope is the assembled, tier-ordered context (§8.5). Elements are kept
// tier-grouped with authoritative first; the injection contract relies on
// that ordering (must-include placed first, never truncated).
type Envelope struct {
	Elements []Element `json:"elements"`
}

// ElementsInTier returns the elements carrying the given tier, in envelope
// order.
func (e *Envelope) ElementsInTier(tier TrustTier) []Element {
	var out []Element
	for _, el := range e.Elements {
		if el.Tier == tier {
			out = append(out, el)
		}
	}
	return out
}

// HasTier reports whether any element carries the tier.
func (e *Envelope) HasTier(tier TrustTier) bool {
	for _, el := range e.Elements {
		if el.Tier == tier {
			return true
		}
	}
	return false
}

// envelopeBuilder is the sole construction path for Envelopes (the
// "assembled by the control plane, never the agent" invariant made
// structural: the Assembler hands out a builder, nothing else creates
// envelopes).
type envelopeBuilder struct {
	elements []Element
}

// newEnvelopeBuilder returns a fresh builder.
func newEnvelopeBuilder() *envelopeBuilder { return &envelopeBuilder{} }

// add appends a tier-stamped element. The tier comes from the CALLING site's
// knowledge of the source — there is no API that reads a tier out of content.
func (b *envelopeBuilder) add(tier TrustTier, kind, content string, prov Provenance, score float64) {
	b.elements = append(b.elements, Element{
		Tier:       tier,
		Kind:       kind,
		Content:    content,
		Provenance: prov,
		Score:      score,
	})
}

// addAuthoritative appends an authoritative-tier element (work item / AC /
// goals). Must-include content — the budgeter never truncates it (5.9).
func (b *envelopeBuilder) addAuthoritative(kind, content string, prov Provenance) {
	b.add(TierAuthoritative, kind, content, prov, 0)
}

// addProjectMeta appends project-metadata content (repo/ref, arch-doc refs,
// conventions) to the authoritative tier: it is control-plane-sourced, but it
// is best-effort budget-wise (projectDocs tier, 5.9) — truncatable after the
// must-include set, ahead of recall/artifacts.
func (b *envelopeBuilder) addProjectMeta(kind, content string, prov Provenance) {
	b.add(TierAuthoritative, kind, content, prov, 0)
}

// addUntrustedRecall appends a memory-recall element with its §7.3
// provenance. Score is the relevance (distance-derived); higher survives the
// Run dynamic trim longer.
func (b *envelopeBuilder) addUntrustedRecall(kind, content string, prov Provenance, score float64) {
	b.add(TierUntrustedRecall, kind, content, prov, score)
}

// addUntrustedExternal appends synced repo/PR/artifact content (D8).
func (b *envelopeBuilder) addUntrustedExternal(kind, content string, prov Provenance) {
	b.add(TierUntrustedExternal, kind, content, prov, 0)
}

// build finalizes the envelope: tier-grouped with authoritative first, and
// within the untrusted-recall tier relevance-ordered (score desc, stable) so
// the budgeter can drop low-scoring recall before high (§8.5 Run trim).
func (b *envelopeBuilder) build() *Envelope {
	order := map[TrustTier]int{TierAuthoritative: 0, TierUntrustedRecall: 1, TierUntrustedExternal: 2}
	els := make([]Element, len(b.elements))
	copy(els, b.elements)
	sort.SliceStable(els, func(i, j int) bool {
		if order[els[i].Tier] != order[els[j].Tier] {
			return order[els[i].Tier] < order[els[j].Tier]
		}
		if els[i].Tier == TierUntrustedRecall && els[i].Score != els[j].Score {
			return els[i].Score > els[j].Score // relevance desc: keep high-scoring recall first
		}
		return false
	})
	return &Envelope{Elements: els}
}

// String renders a stable, human-auditable dump of the envelope (audit tool,
// not the injection path — the shim-facing rendering is inject.go).
func (e *Envelope) String() string {
	var sb strings.Builder
	for _, el := range e.Elements {
		fmt.Fprintf(&sb, "[%s/%s] %s\n", el.Tier, el.Kind, el.Content)
	}
	return sb.String()
}
