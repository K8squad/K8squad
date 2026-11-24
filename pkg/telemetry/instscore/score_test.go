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
	"math"
	"testing"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestScoreSpecWorkedExample(t *testing.T) {
	// The specification's worked example (§Score Calculation Formula):
	// Critical 4/8, Important 8/10, Normal 6/8, Low 1/5 → 530/830 ≈ 63.86.
	passed := map[Impact]int{ImpactCritical: 4, ImpactImportant: 8, ImpactNormal: 6, ImpactLow: 1}
	totals := map[Impact]int{ImpactCritical: 8, ImpactImportant: 10, ImpactNormal: 8, ImpactLow: 5}
	got := roundScore(Score(passed, totals))
	if got != 63.86 {
		t.Fatalf("spec worked example: got %.4f, want 63.86", got)
	}
	if math.Abs(Score(passed, totals)-530.0/830.0*100) > 1e-9 {
		t.Fatal("unrounded score deviates from 530/830*100")
	}
}

func TestScoreExtremesAndWeights(t *testing.T) {
	all := map[Impact]int{ImpactCritical: 5, ImpactImportant: 13, ImpactNormal: 7, ImpactLow: 1}
	if got := Score(all, all); got != 100 {
		t.Fatalf("all-pass: got %v, want 100", got)
	}
	none := map[Impact]int{}
	if got := Score(none, all); got != 0 {
		t.Fatalf("all-fail: got %v, want 0", got)
	}
	// Weight property: passing every Critical rule only must beat passing
	// every Low rule only, with identical totals.
	onlyCritical := map[Impact]int{ImpactCritical: 5}
	onlyLow := map[Impact]int{ImpactLow: 1}
	if Score(onlyCritical, all) <= Score(onlyLow, all) {
		t.Fatal("Critical-only pass should outweigh Low-only pass")
	}
	// Exact weight arithmetic: one Critical pass of one = 40/ (5*40+13*30+7*20+1*10)
	want := 40.0 / float64(5*40+13*30+7*20+1*10) * 100
	if got := Score(map[Impact]int{ImpactCritical: 1}, all); math.Abs(got-want) > 1e-9 {
		t.Fatalf("weight arithmetic: got %v want %v", got, want)
	}
	// Empty registry scores 0, not NaN.
	if got := Score(nil, nil); got != 0 || math.IsNaN(got) {
		t.Fatalf("empty: got %v", got)
	}
	// Pass counts above totals are clamped defensively.
	if got := Score(map[Impact]int{ImpactLow: 99}, map[Impact]int{ImpactLow: 1}); got != 100 {
		t.Fatalf("clamp: got %v", got)
	}
}

func TestCategorizeBoundaries(t *testing.T) {
	cases := []struct {
		score float64
		want  Category
	}{
		{100, CategoryExcellent},
		{90, CategoryExcellent},
		{89.99, CategoryGood},
		{75, CategoryGood},
		{74.99, CategoryNeedsImprovement},
		{50, CategoryNeedsImprovement},
		{49.99, CategoryPoor},
		{0, CategoryPoor},
	}
	for _, c := range cases {
		if got := Categorize(c.score); got != c.want {
			t.Fatalf("Categorize(%v) = %q, want %q", c.score, got, c.want)
		}
	}
}

func TestEvaluatePerfectWindow(t *testing.T) {
	acc := NewAccumulator()
	r := goodResource("svc", "i-1", "pod-1", "uid-1")
	// Spans: one trace, server root + client child, all ≥ 5ms.
	acc.AcceptTraces(traceReq(spansReq(r,
		span("tr1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "GET /pay", ns(1000), ns(1100)),
		span("tr1", "s2", "s1", tracepb.Span_SPAN_KIND_CLIENT, "GET upstream", ns(1000), ns(1100)))))
	acc.AcceptLogs(logReq2(logsReq(r,
		logRecord(ns(1000), "INFO", 9, "request handled"))))
	acc.AcceptMetrics(metricsReq(metricsReqFor(r,
		gaugeMetric("queue.depth", "{obj}", ns(1000)),
		histMetric("request.duration", "ms", ns(1000), []float64{1, 5, 10}))))

	res := Evaluate(acc.Window("svc"))
	if res.Score != 100 {
		t.Fatalf("perfect window score: got %v, want 100 (failures: %+v)", res.Score, res.Failed())
	}
	if res.Category != CategoryExcellent {
		t.Fatalf("category: %q", res.Category)
	}
	if res.Total != 26 {
		t.Fatalf("total: %d", res.Total)
	}
	if res.Implemented != 24 {
		t.Fatalf("implemented: %d, want 24 (SPA-003 + SDK-001 excluded)", res.Implemented)
	}
	wantNE := map[string]bool{"SPA-003": true, "SDK-001": true}
	if len(res.NotEvaluated) != 2 {
		t.Fatalf("not evaluated: %v", res.NotEvaluated)
	}
	for _, id := range res.NotEvaluated {
		if !wantNE[id] {
			t.Fatalf("unexpected not-evaluated rule %s", id)
		}
	}
	if len(res.Verdicts) != 26 {
		t.Fatalf("verdicts: %d", len(res.Verdicts))
	}
}

func TestEvaluateNoDataFailsTargetRules(t *testing.T) {
	acc := NewAccumulator()
	acc.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"), gaugeMetric("queue.depth", "{obj}", ns(1)))))
	res := Evaluate(acc.Window("svc"))

	failed := map[string]bool{}
	for _, v := range res.Failed() {
		failed[v.Rule] = true
	}
	for _, id := range []string{"LOG-001", "LOG-002", "LOG-003", "SPA-001", "SPA-002", "SPA-004", "SPA-005", "SPA-006"} {
		if !failed[id] {
			t.Errorf("rule %s should fail on missing target signal", id)
		}
	}
	for _, id := range []string{"MET-001", "MET-002", "MET-003", "RES-005", "RES-007"} {
		if failed[id] {
			t.Errorf("rule %s should not fail on a metrics-only window: %s", id, verdictOf(res, id).Evidence)
		}
	}
}

func verdictOf(r Result, id string) Verdict {
	for _, v := range r.Verdicts {
		if v.Rule == id {
			return v
		}
	}
	return Verdict{}
}

func TestEvaluateEmptyWindow(t *testing.T) {
	res := Evaluate(&Window{})
	if res.Score != 0 || res.Category != CategoryPoor || res.Implemented != 24 {
		t.Fatalf("empty window: %+v", res)
	}
	if len(res.Failed()) != 24 {
		t.Fatalf("empty window should fail all 24 evaluable rules, got %d", len(res.Failed()))
	}
}

func TestEveryRuleHasEvaluator(t *testing.T) {
	for _, r := range Rules {
		if _, ok := evaluators[r.ID]; !ok {
			t.Errorf("rule %s has no evaluator", r.ID)
		}
	}
	if len(evaluators) != len(Rules) {
		t.Errorf("evaluator count %d != registry count %d (stale entry?)", len(evaluators), len(Rules))
	}
}

func TestResultString(t *testing.T) {
	acc := NewAccumulator()
	acc.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"), gaugeMetric("queue.depth", "{obj}", ns(1)))))
	s := Evaluate(acc.Window("svc")).String()
	if s == "" || len(s) < 10 {
		t.Fatalf("summary string: %q", s)
	}
}
