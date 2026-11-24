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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Fixture-driven end-to-end tests: OTLP JSON dumps (protojson at the
// collector wire boundary) → LoadDump → windows → Evaluate. These pin the
// whole pipeline the cmd/instscore CLI runs, including the spec formula on
// realistic pass/fail mixes.

func loadFixture(t *testing.T, name string) *Accumulator {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	acc, err := LoadDump(data)
	if err != nil {
		t.Fatalf("load fixture %s: %v", name, err)
	}
	return acc
}

func TestFixtureGoodServiceScores100(t *testing.T) {
	acc := loadFixture(t, "good_service.json")
	w := acc.Window("payments")
	if w == nil {
		t.Fatal("payments window missing")
	}
	res := Evaluate(w)
	if res.Score != 100 || res.Category != CategoryExcellent {
		t.Fatalf("good fixture: %s", res.String())
	}
	if res.Implemented != 24 || res.Total != 26 {
		t.Fatalf("disclosure: implemented=%d total=%d", res.Implemented, res.Total)
	}
	for _, v := range res.Verdicts {
		if v.Status == StatusFail {
			t.Errorf("unexpected failure %s: %s", v.Rule, v.Evidence)
		}
	}
}

func TestFixtureViolations(t *testing.T) {
	acc := loadFixture(t, "violations.json")
	res := Evaluate(acc.Window("broken"))

	// Expected verdict per rule for this fixture. NE = not_evaluated.
	want := map[string]Status{
		"LOG-001": StatusFail, "LOG-002": StatusFail, "LOG-003": StatusFail,
		"MET-001": StatusPass, "MET-002": StatusFail, "MET-003": StatusFail,
		"MET-004": StatusFail, "MET-005": StatusFail, "MET-006": StatusFail,
		"MET-007": StatusFail, "MET-008": StatusFail,
		"RES-001": StatusPass, "RES-002": StatusFail, "RES-003": StatusFail,
		"RES-004": StatusFail, "RES-005": StatusPass, "RES-006": StatusFail,
		"RES-007": StatusPass, "RES-010": StatusFail,
		"SDK-001": StatusNotEvaluated,
		"SPA-001": StatusFail, "SPA-002": StatusFail, "SPA-003": StatusNotEvaluated,
		"SPA-004": StatusFail, "SPA-005": StatusFail, "SPA-006": StatusFail,
	}
	for _, v := range res.Verdicts {
		if want[v.Rule] != v.Status {
			t.Errorf("%s: got %q, want %q (%s)", v.Rule, v.Status, want[v.Rule], v.Evidence)
		}
	}

	// Formula on this mix: Critical 1/5, Important 2/12, Normal 1/7 →
	// (40 + 60 + 20) / (200 + 360 + 140) × 100 = 17.14 (Poor).
	if res.Score != 17.14 {
		t.Fatalf("violations score: got %.4f, want 17.14", res.Score)
	}
	if res.Category != CategoryPoor {
		t.Fatalf("category: %q", res.Category)
	}
}

func TestFixtureNoDataMetricsOnly(t *testing.T) {
	acc := loadFixture(t, "no_data_metrics_only.json")
	res := Evaluate(acc.Window("metrics-only"))

	// Signal-absent rules fail with no-data evidence.
	for _, id := range []string{"LOG-001", "LOG-002", "LOG-003", "SPA-001", "SPA-002", "SPA-004", "SPA-005", "SPA-006", "RES-004"} {
		v := verdictOf(res, id)
		if v.Status != StatusFail {
			t.Errorf("%s: got %q, want fail", id, v.Status)
			continue
		}
		wantSignal := "Log"
		if id != "LOG-001" && id != "LOG-002" && id != "LOG-003" {
			wantSignal = "Span"
		}
		if id == "RES-004" {
			wantSignal = "Log, Span"
		}
		if got := v.Evidence; !contains(got, "no "+wantSignal+" observed in window") {
			t.Errorf("%s evidence %q, want no-data for %s", id, got, wantSignal)
		}
	}
	// Everything evaluable on metrics+resources passes.
	// Critical 3/5, Important 7/12, Normal 5/7 →
	// (120 + 210 + 100) / 700 × 100 = 61.43 (Needs Improvement).
	if res.Score != 61.43 {
		t.Fatalf("no-data score: got %.4f, want 61.43", res.Score)
	}
	if res.Category != CategoryNeedsImprovement {
		t.Fatalf("category: %q", res.Category)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestLoadDumpSingleSignalBareRequest(t *testing.T) {
	// The metrics-only fixture is also a valid bare single-signal shape:
	// unwrap the single element of "metrics" into a top-level request.
	data, err := os.ReadFile(filepath.Join("testdata", "no_data_metrics_only.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper struct {
		Metrics []json.RawMessage `json:"metrics"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil || len(wrapper.Metrics) != 1 {
		t.Fatalf("unwrap precondition failed: %v (n=%d)", err, len(wrapper.Metrics))
	}
	acc, err := LoadDump(wrapper.Metrics[0])
	if err != nil {
		t.Fatalf("bare single-signal dump: %v", err)
	}
	if acc.Window("metrics-only") == nil {
		t.Fatal("metrics-only window missing from bare dump")
	}
}

func TestLoadDumpRejectsGarbage(t *testing.T) {
	for _, in := range []string{``, `[]`, `{"nonsense": 1}`, `{"traces": "not-an-array"}`, `{`} {
		if _, err := LoadDump([]byte(in)); err == nil {
			t.Errorf("LoadDump(%q) should fail", in)
		}
	}
}
