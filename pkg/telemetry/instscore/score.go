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

package instscore

import (
	"fmt"
	"math"
	"sort"
)

// Status is a rule's evaluation outcome inside a window.
type Status string

const (
	// StatusPass: the rule's criteria held on all observed evidence.
	StatusPass Status = "pass"
	// StatusFail: observed evidence violates the criteria, or the rule's
	// target signal was entirely absent (the spec has no N/A; see the
	// no-data policy in the package README).
	StatusFail Status = "fail"
	// StatusNotEvaluated: the rule is registered but excluded from this
	// score because it cannot be deterministically evaluated from OTLP
	// evidence at the vendored pin (SPA-003's criteria is an upstream TODO;
	// SDK-001 needs an SDK support matrix). The specification requires
	// implementations that do not implement all rules to disclose it —
	// Result.Implemented/Total and NotEvaluated carry that disclosure.
	StatusNotEvaluated Status = "not_evaluated"
)

// Category is the spec's qualitative band for a score.
type Category string

const (
	CategoryExcellent        Category = "Excellent"
	CategoryGood             Category = "Good"
	CategoryNeedsImprovement Category = "Needs Improvement"
	CategoryPoor             Category = "Poor"
)

// Categorize maps a 0-100 score to the spec's qualitative categories:
// 90-100 Excellent, 75-89 Good, 50-74 Needs Improvement, 0-49 Poor.
func Categorize(score float64) Category {
	switch {
	case score >= 90:
		return CategoryExcellent
	case score >= 75:
		return CategoryGood
	case score >= 50:
		return CategoryNeedsImprovement
	default:
		return CategoryPoor
	}
}

// Verdict is one rule's result against a window.
type Verdict struct {
	// Rule is the rule ID (e.g. "RES-005").
	Rule string
	// Status is pass, fail, or not_evaluated.
	Status Status
	// Evidence explains the outcome: what was observed (counts, offending
	// keys, sample values) or why the rule was not evaluated. Deterministic.
	Evidence string
}

// Result is the full evaluation outcome for one service window.
type Result struct {
	// ServiceName is the evaluated service.name ("" when absent).
	ServiceName string
	// Score is the instrumentation score, 0-100, rounded to 2 decimals.
	Score float64
	// Category is the qualitative band of Score.
	Category Category
	// Implemented is the number of registry rules that entered the formula.
	Implemented int
	// Total is the registry size (26 at the vendored pin).
	Total int
	// NotEvaluated lists the rule IDs excluded from the formula, with the
	// exclusion reason in their verdicts. Spec-mandated disclosure.
	NotEvaluated []string
	// Verdicts holds one verdict per registry rule, sorted by rule ID.
	Verdicts []Verdict
}

// Score computes the instrumentation score from per-impact-level pass counts,
// exactly as the specification defines it:
//
//	Score = Σ(P_i × W_i) / Σ(T_i × W_i) × 100
//
// with weights Critical=40, Important=30, Normal=20, Low=10. Rules with a
// zero total for their level are skipped; an all-zero input (no rules) scores
// 0. The result is unrounded — Result.Score applies the presentation rounding.
func Score(passed, totals map[Impact]int) float64 {
	var num, den int
	for _, level := range []Impact{ImpactCritical, ImpactImportant, ImpactNormal, ImpactLow} {
		t := totals[level]
		if t == 0 {
			continue
		}
		p := passed[level]
		if p > t {
			p = t
		}
		num += p * level.Weight()
		den += t * level.Weight()
	}
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den) * 100
}

// Evaluate runs every registered rule evaluator against the window and
// computes the score. It is pure: the same window always yields the same
// result.
func Evaluate(w *Window) Result {
	res := Result{
		ServiceName: w.ServiceName,
		Total:       len(Rules),
		Verdicts:    make([]Verdict, 0, len(Rules)),
	}

	passed := map[Impact]int{}
	totals := map[Impact]int{}

	rules := make([]Rule, len(Rules))
	copy(rules, Rules)
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })

	for _, rule := range rules {
		v := Verdict{Rule: rule.ID}
		eval, ok := evaluators[rule.ID]
		switch {
		case !ok:
			// A registry entry without an evaluator is a programming error
			// surfaced as not_evaluated rather than silently inflating the
			// score; the eval_test "every rule has an evaluator" guard makes
			// this branch unreachable in practice.
			v.Status = StatusNotEvaluated
			v.Evidence = "no evaluator registered"
		default:
			v = eval(w)
			v.Rule = rule.ID
		}

		switch v.Status {
		case StatusNotEvaluated:
			res.NotEvaluated = append(res.NotEvaluated, rule.ID)
		case StatusPass, StatusFail:
			res.Implemented++
			totals[rule.Impact]++
			if v.Status == StatusPass {
				passed[rule.Impact]++
			}
		}

		res.Verdicts = append(res.Verdicts, v)
	}

	res.Score = roundScore(Score(passed, totals))
	res.Category = Categorize(res.Score)
	return res
}

// roundScore rounds to two decimals (spec worked example: 63.855... → 63.86).
func roundScore(v float64) float64 {
	return math.Round(v*100) / 100
}

// Failed returns the failing verdicts, sorted by rule ID.
func (r Result) Failed() []Verdict {
	var out []Verdict
	for _, v := range r.Verdicts {
		if v.Status == StatusFail {
			out = append(out, v)
		}
	}
	return out
}

// String renders a one-line summary for reports and logs.
func (r Result) String() string {
	return fmt.Sprintf("%s: %.2f (%s), %d/%d rules implemented", orUnnamed(r.ServiceName), r.Score, r.Category, r.Implemented, r.Total)
}

func orUnnamed(name string) string {
	if name == "" {
		return "(no service.name)"
	}
	return name
}
