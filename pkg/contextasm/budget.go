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

package contextasm

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ============================================================================
// Story 5.9 — the model-window token budget
// ============================================================================
//
// The budget is keyed to the resolved MODEL window (§10.1 Agent Card
// contextWindow — Claude ~200K vs BYO Ollama ~8K), not the runtime CLI. It is
// hierarchical and operator-tunable without code changes (§8.5):
//
//	Project-level default → Agent-level override → Run-level dynamic trim,
//	the whole thing CLAMPED by the model window — configuration can shrink
//	the budget but never exceed the physical window.
//
// Application is priority-ordered: must-include (work item + acceptance
// criteria + goals) is placed first and NEVER truncated; best-effort tiers
// are truncated to fit, lowest-priority first. If must-include alone exceeds
// the window the Run FAILS CLOSED — never silent truncation of the task
// itself.

// Budget is the resolved per-tier allocation the Assembler applies.
type Budget struct {
	// WorkItem caps the must-include tier (work item + AC + goals). The cap
	// is advisory for this tier — must-include is never truncated; an
	// over-cap must-include that still fits the window passes through.
	WorkItem int64
	// ProjectDocs caps project metadata (arch docs, conventions).
	ProjectDocs int64
	// MemoryRecall caps the untrusted-recall tier.
	MemoryRecall int64
	// Artifacts caps the untrusted-external artifact elements.
	Artifacts int64
}

// ErrBudgetAboveWindow is the fail-closed validation error for a configured
// budget tier exceeding the resolved model contextWindow (§8.5: "a
// contextBudgetOverride above the model window is a fail-closed validation
// error, not a silent overflow"). The reconciler surfaces it as a Run
// condition; the envelope is never assembled against an over-window budget.
var ErrBudgetAboveWindow = errors.New("context budget tier exceeds the resolved model contextWindow (fail-closed, §8.5)")

// ErrMustIncludeExceedsWindow is the fail-closed assembly error for story
// 5.9: the must-include content (work item + acceptance criteria + goals)
// alone exceeds the model window — a too-small local model. The Run fails
// with a clear condition; the task itself is NEVER silently truncated.
var ErrMustIncludeExceedsWindow = errors.New("must-include context (work item + acceptance criteria + goals) exceeds the model contextWindow — Run fails closed, never silent truncation (story 5.9)")

// ResolveBudget implements the §8.5 three-layer resolution:
//
//	Project default → Agent override → (Run dynamic trim happens at Apply)
//	clamped by the resolved model contextWindow.
//
// Unset tiers inherit from the next level up; a tier set ABOVE the window is
// returned as ErrBudgetAboveWindow (fail-closed). Tiers clamp DOWN to the
// window silently — configuration may shrink the budget but never exceed the
// physical window (§8.5).
func ResolveBudget(project *ksquadv1alpha1.ContextBudget, agentOverride *ksquadv1alpha1.ContextBudget, contextWindow int64) (Budget, error) {
	if contextWindow <= 0 {
		return Budget{}, fmt.Errorf("ResolveBudget: contextWindow must be positive (model-keyed capability, §10.1), got %d", contextWindow)
	}
	resolved := Budget{}
	for _, t := range []struct {
		project  *int64
		override *int64
		dst      *int64
		name     string
	}{
		{deref(project).WorkItem, deref(agentOverride).WorkItem, &resolved.WorkItem, "workItem"},
		{deref(project).ProjectDocs, deref(agentOverride).ProjectDocs, &resolved.ProjectDocs, "projectDocs"},
		{deref(project).MemoryRecall, deref(agentOverride).MemoryRecall, &resolved.MemoryRecall, "memoryRecall"},
		{deref(project).Artifacts, deref(agentOverride).Artifacts, &resolved.Artifacts, "artifacts"},
	} {
		v := firstSet(t.project, t.override)
		if v == nil {
			continue // tier unset everywhere: no cap beyond the window clamp
		}
		if *v > contextWindow {
			return Budget{}, fmt.Errorf("%w: tier %s=%d > window=%d", ErrBudgetAboveWindow, t.name, *v, contextWindow)
		}
		*t.dst = *v
	}
	return resolved, nil
}

// firstSet returns the override if set, else the project default (Agent
// override wins per §8.5; the Run trim is dynamic and not a config layer).
func firstSet(project, override *int64) *int64 {
	if override != nil {
		return override
	}
	return project
}

func deref(b *ksquadv1alpha1.ContextBudget) ksquadv1alpha1.ContextBudget {
	if b == nil {
		return ksquadv1alpha1.ContextBudget{}
	}
	return *b
}

// ApplyBudget enforces the priority-ordered model-window budget on the
// envelope (story 5.9). It mutates a copy and returns the budgeted envelope:
//
//   - must-include elements (authoritative kinds description /
//     acceptanceCriteria / goal, plus workItem comments) are placed first and
//     NEVER truncated;
//   - projectMeta (authoritative tier, best-effort) is truncated only after
//     must-include fits;
//   - untrusted tiers truncate lowest-priority first (artifacts before
//     recall); recall drops LOW-scoring elements before high (Run dynamic
//     trim, §8.5);
//   - if must-include ALONE exceeds the window, ErrMustIncludeExceedsWindow
//     is returned (fail-closed, clear condition — the reconciler fails the
//     Run rather than truncate the task).
//
// overheadTokens (the injection framing's own cost, inject.go) is reserved
// off the window before anything is placed.
func ApplyBudget(env *Envelope, b Budget, contextWindow int64, overheadTokens int64) (*Envelope, error) {
	if env == nil {
		return nil, errors.New("ApplyBudget: nil envelope")
	}
	if contextWindow <= 0 {
		return nil, fmt.Errorf("ApplyBudget: contextWindow must be positive, got %d", contextWindow)
	}
	if overheadTokens < 0 {
		return nil, fmt.Errorf("ApplyBudget: overheadTokens must be >= 0, got %d", overheadTokens)
	}

	out := &Envelope{Elements: make([]Element, len(env.Elements))}
	copy(out.Elements, env.Elements)

	window := contextWindow - overheadTokens
	if window <= 0 {
		return nil, fmt.Errorf("%w: injection framing overhead %d leaves no window of %d", ErrMustIncludeExceedsWindow, overheadTokens, contextWindow)
	}

	// 1. Must-include first, never truncated — fail closed if it alone
	//    exceeds the window (after reserving the framing overhead).
	var mustTokens int64
	for _, el := range out.Elements {
		if isMustInclude(el) {
			mustTokens += EstimateTokens(el.Content)
		}
	}
	if mustTokens > window {
		return nil, fmt.Errorf("%w: must-include %d tokens > window %d (model too small for the task)", ErrMustIncludeExceedsWindow, mustTokens, window)
	}
	remaining := window - mustTokens

	// 2. Best-effort tiers, highest priority first, each capped by
	//    min(tier budget, remaining). Within a tier, elements arrive in
	//    relevance order (envelope builder sorts recall by score desc), so
	//    filling from the front drops low-scoring/low-priority content
	//    first.
	for _, group := range []struct {
		match func(Element) bool
		capf  func(Budget) int64
	}{
		{isProjectMeta, func(b Budget) int64 { return b.ProjectDocs }},
		{isUntrustedRecall, func(b Budget) int64 { return b.MemoryRecall }},
		{isUntrustedExternal, func(b Budget) int64 { return b.Artifacts }},
	} {
		var tierTokens int64
		for i := range out.Elements {
			el := &out.Elements[i]
			if !group.match(*el) {
				continue
			}
			cap := effectiveCap(group.capf(b), window)
			room := min64(cap-tierTokens, remaining)
			if room <= 0 {
				el.Content = truncateMarker
				el.Truncated = true
				continue
			}
			full := EstimateTokens(el.Content)
			if full <= room {
				tierTokens += full
				remaining -= full
				continue
			}
			el.Content = truncateToTokens(el.Content, room)
			el.Truncated = true
			used := EstimateTokens(el.Content)
			tierTokens += used
			remaining -= used
		}
	}
	return out, nil
}

// isMustInclude classifies the 5.9 never-truncate set: work item description,
// acceptance criteria, goals, and the work-item comment history (§8.5 work
// item "description, acceptance criteria, comment history"). Everything the
// task itself depends on.
func isMustInclude(el Element) bool {
	if el.Tier != TierAuthoritative {
		return false
	}
	switch el.Kind {
	case "description", "acceptanceCriteria", "goal", "comment":
		return true
	}
	return false
}

// isProjectMeta is the authoritative-tier best-effort class (arch docs,
// conventions, repo metadata): truncatable, but only after must-include.
func isProjectMeta(el Element) bool {
	return el.Tier == TierAuthoritative && !isMustInclude(el)
}

func isUntrustedRecall(el Element) bool   { return el.Tier == TierUntrustedRecall }
func isUntrustedExternal(el Element) bool { return el.Tier == TierUntrustedExternal }

func effectiveCap(set int64, window int64) int64 {
	if set > 0 {
		return set
	}
	return window
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// truncateMarker replaces content dropped entirely by the budgeter.
const truncateMarker = "[dropped: context budget exhausted for this tier]"

// truncateToTokens shortens content to at most n estimated tokens, marking
// the truncation so the model knows material was cut (never silently).
func truncateToTokens(content string, n int64) string {
	if n <= 0 {
		return truncateMarker
	}
	tokens := tokenize(content)
	if int64(len(tokens)) <= n {
		return content
	}
	kept := strings.Join(tokens[:n], " ")
	return kept + " …[truncated to fit the context budget]"
}

// ============================================================================
// EstimateTokens — the deterministic token estimator seam
// ============================================================================

// EstimateTokens estimates the model-token cost of s. It is a deterministic,
// dependency-free heuristic (whitespace-delimited words + punctuation
// clusters + a ceil on CJK runes, which tokenize roughly one-per-character):
// the SAME estimator is used for budget resolution, application and framing
// overhead, so every comparison is consistent. Model-specific tokenizers
// plug in behind this seam without touching the budget logic (the number is
// an estimate; the fail-closed guards err by counting generously).
func EstimateTokens(s string) int64 {
	var words, punct, cjk int64
	inWord := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			inWord = false
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			cjk++
			inWord = false
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			punct++
			inWord = false
		default:
			if !inWord {
				words++
				inWord = true
			}
		}
	}
	// ~4 punctuation marks ≈ 1 token (BPE merges most); CJK ≈ 1/char.
	return words + (punct+3)/4 + cjk
}

// tokenize splits on whitespace runs (the estimator's word unit).
func tokenize(s string) []string {
	return strings.Fields(s)
}
