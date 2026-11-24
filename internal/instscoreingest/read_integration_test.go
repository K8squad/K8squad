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

package instscoreingest

import (
	"context"
	"testing"
	"time"
)

// TestReadModelListAndDetail (ISI-7750, phase 4) exercises the read surface
// against the real schema: the services list aggregate (evidence counts +
// latest score + verdict tallies, worst-score-first) and the per-service
// detail (verdict table + hourly history, oldest first). Gated on DATABASE_URL
// like the sibling integration tests; service names are unique to this test
// and cleaned up so re-runs stay deterministic.
func TestReadModelListAndDetail(t *testing.T) {
	dbh := openTestDB(t)
	store, err := NewSQLStore(dbh)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	ctx := context.Background()

	const svcPoor, svcGood, svcPending, svcGone = "rd-poor", "rd-good", "rd-pending", "rd-never-seen"
	const unsetName = "" // the missing-service.name bucket must stay visible
	defer func() {
		for _, name := range []string{svcPoor, svcGood, svcPending, unsetName} {
			for _, table := range []string{"evidence_window", "rule_verdict", "score_history"} {
				if _, err := dbh.ExecContext(ctx,
					`DELETE FROM instscore.`+table+` WHERE service_name=$1`, name); err != nil {
					t.Fatalf("cleanup %s/%s: %v", table, name, err)
				}
			}
		}
	}()

	now := time.Now().UTC()
	// Two evaluated services + one that only just arrived (mid-throttle) + one
	// evidence row with no service.name at all.
	if err := store.AppendWindow(ctx, []WindowRow{
		{ServiceName: svcPoor, Signal: "traces", Evidence: []byte(`{"resourceSpans":[]}`), ReceivedAt: now.Add(-2 * time.Hour), SpanCount: 10, ResourceCount: 3},
		{ServiceName: svcPoor, Signal: "logs", Evidence: []byte(`{"resourceLogs":[]}`), ReceivedAt: now.Add(-90 * time.Minute), LogCount: 5},
		{ServiceName: svcGood, Signal: "traces", Evidence: []byte(`{"resourceSpans":[]}`), ReceivedAt: now.Add(-30 * time.Minute), SpanCount: 4, ResourceCount: 1},
		{ServiceName: svcPending, Signal: "traces", Evidence: []byte(`{"resourceSpans":[]}`), ReceivedAt: now.Add(-time.Minute), SpanCount: 1, ResourceCount: 1},
		{ServiceName: unsetName, Signal: "traces", Evidence: []byte(`{"resourceSpans":[]}`), ReceivedAt: now.Add(-5 * time.Minute), SpanCount: 2, ResourceCount: 1},
	}); err != nil {
		t.Fatalf("AppendWindow: %v", err)
	}

	poorVerdicts := []VerdictRow{
		{RuleID: "RES-005", Status: "pass", Evidence: "service.name present", Impact: "Critical"},
		{RuleID: "LOG-001", Status: "fail", Evidence: "debug logs observed", Impact: "Important"},
		{RuleID: "SPA-003", Status: "not_evaluated", Evidence: "upstream TODO at pin", Impact: "Important"},
	}
	goodVerdicts := []VerdictRow{
		{RuleID: "RES-005", Status: "pass", Evidence: "service.name present", Impact: "Critical"},
		{RuleID: "LOG-001", Status: "pass", Evidence: "no debug logs", Impact: "Important"},
	}
	save := func(svc string, verdicts []VerdictRow, hourOffset time.Duration, score float64, category string) {
		t.Helper()
		at := now.Add(hourOffset)
		if err := store.SaveEvaluation(ctx, svc, verdicts, ScoreRow{
			ServiceName: svc, BucketHour: at.Truncate(time.Hour), Score: score,
			Category: category, Implemented: 24, Total: 26, EvaluatedAt: at,
		}); err != nil {
			t.Fatalf("SaveEvaluation(%s): %v", svc, err)
		}
	}
	save(svcPoor, poorVerdicts, -2*time.Hour, 17.14, "Poor")
	save(svcPoor, poorVerdicts, -1*time.Hour, 18.00, "Poor") // same bucket hour on most runs; newest wins
	save(svcGood, goodVerdicts, -30*time.Minute, 96.43, "Excellent")

	// ── List ──
	list, err := store.ListServices(ctx)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	byName := make(map[string]ServiceSummary, len(list))
	var order []string
	for _, sm := range list {
		if sm.ServiceName == svcGone {
			t.Fatalf("never-seen service must not appear in the list")
		}
		if _, dup := byName[sm.ServiceName]; !dup {
			order = append(order, sm.ServiceName)
		}
		byName[sm.ServiceName] = sm
	}

	poor, ok := byName[svcPoor]
	if !ok {
		t.Fatalf("%s missing from list (%v)", svcPoor, order)
	}
	if poor.Score == nil || *poor.Score < 17 || *poor.Score > 19 {
		t.Fatalf("poor score = %v, want the ~18 latest point", poor.Score)
	}
	if poor.Category != "Poor" || poor.Implemented != 24 || poor.Total != 26 {
		t.Fatalf("poor summary drift: %+v", poor)
	}
	if poor.PassCount != 1 || poor.FailCount != 1 || poor.SkipCount != 1 {
		t.Fatalf("poor tallies = pass %d fail %d skip %d, want 1/1/1", poor.PassCount, poor.FailCount, poor.SkipCount)
	}
	if poor.SpanCount != 10 || poor.LogCount != 5 || poor.ResourceCount != 3 {
		t.Fatalf("poor evidence counts drift: %+v", poor)
	}

	pending, ok := byName[svcPending]
	if !ok || pending.Score != nil || pending.Category != "" || pending.EvaluatedAt != nil {
		t.Fatalf("mid-throttle service must list with a NULL score: %+v ok=%v", pending, ok)
	}
	if _, ok := byName[unsetName]; !ok {
		t.Fatalf("the empty service.name bucket must stay visible in the list (RES-005 evidence)")
	}

	// Ordering: worst score first; the NULL-score pending bucket sorts last.
	if len(order) > 0 && order[0] != svcPoor {
		t.Fatalf("list order = %v, want %s (worst score) first", order, svcPoor)
	}

	// ── Detail ──
	detail, found, err := store.GetService(ctx, svcPoor)
	if err != nil || !found {
		t.Fatalf("GetService(%s): %v found=%v", svcPoor, err, found)
	}
	if len(detail.Verdicts) != 3 {
		t.Fatalf("verdicts = %d, want 3", len(detail.Verdicts))
	}
	if detail.Verdicts[0].RuleID != "LOG-001" || detail.Verdicts[1].RuleID != "RES-005" || detail.Verdicts[2].RuleID != "SPA-003" {
		t.Fatalf("verdict order = %s,%s,%s, want rule-ID order", detail.Verdicts[0].RuleID, detail.Verdicts[1].RuleID, detail.Verdicts[2].RuleID)
	}
	if detail.Verdicts[2].Status != "not_evaluated" || detail.Verdicts[2].Impact != "Important" {
		t.Fatalf("SPA-003 verdict drift: %+v", detail.Verdicts[2])
	}
	if len(detail.History) == 0 || detail.History[0].Score < 17 || detail.History[0].Score > 19 {
		t.Fatalf("history = %+v, want the newest point present oldest-first", detail.History)
	}
	for i := 1; i < len(detail.History); i++ {
		if detail.History[i-1].BucketHour.After(detail.History[i].BucketHour) {
			t.Fatalf("history not oldest-first: %+v", detail.History)
		}
	}

	// The pruned-window service (verdicts/history survive, evidence gone)
	// still resolves.
	if _, err := dbh.ExecContext(ctx,
		`DELETE FROM instscore.evidence_window WHERE service_name=$1`, svcGood); err != nil {
		t.Fatalf("prune rd-good evidence: %v", err)
	}
	detail, found, err = store.GetService(ctx, svcGood)
	if err != nil || !found {
		t.Fatalf("GetService(pruned %s): %v found=%v", svcGood, err, found)
	}
	if detail.Summary.Score == nil || *detail.Summary.Score != 96.43 || detail.Summary.SpanCount != 0 {
		t.Fatalf("pruned-window summary drift: %+v", detail.Summary)
	}

	// Unknown service: not found, no error.
	if _, found, err := store.GetService(ctx, svcGone); err != nil || found {
		t.Fatalf("GetService(unknown) = found=%v err=%v, want false/nil", found, err)
	}
}
