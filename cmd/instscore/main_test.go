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

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const fixtureDir = "../../pkg/telemetry/instscore/testdata"

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	// run writes to an io.Writer; errors are returned for the test to inspect.
	err := run(args, &out)
	return out.String(), err
}

func TestCLIGoodFixtureHumanOutput(t *testing.T) {
	out, err := runCLI(t, fixtureDir+"/good_service.json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{
		"service payments",
		"100.00 / 100 (Excellent)",
		"24/26 rules",
		"SDK-001, SPA-003",
		"pass  RES-005",
		"FAIL  ", // absent in the good fixture: marker defined, no occurrence
	} {
		if want == "FAIL  " {
			if strings.Contains(out, "FAIL") {
				t.Fatalf("good fixture should have no failing rules:\n%s", out)
			}
			continue
		}
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCLIViolationsFixtureHumanOutput(t *testing.T) {
	out, err := runCLI(t, fixtureDir+"/violations.json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{
		"service broken",
		"17.14 / 100 (Poor)",
		"FAIL  LOG-003",
		"FAIL  RES-010",
		"FAIL  SPA-004",
		"n/a   SPA-003",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCLIJSONReport(t *testing.T) {
	out, err := runCLI(t, "-json", fixtureDir+"/violations.json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var rep struct {
		Services []struct {
			ServiceName  string   `json:"service_name"`
			Score        float64  `json:"score"`
			Category     string   `json:"category"`
			Implemented  int      `json:"implemented"`
			Total        int      `json:"total"`
			NotEvaluated []string `json:"not_evaluated"`
			Rules        []struct {
				ID       string `json:"id"`
				Status   string `json:"status"`
				Evidence string `json:"evidence"`
			} `json:"rules"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(rep.Services) != 1 {
		t.Fatalf("services: %d", len(rep.Services))
	}
	s := rep.Services[0]
	if s.ServiceName != "broken" || s.Score != 17.14 || s.Category != "Poor" {
		t.Fatalf("summary: %+v", s)
	}
	if s.Implemented != 24 || s.Total != 26 {
		t.Fatalf("disclosure: %d/%d", s.Implemented, s.Total)
	}
	if len(s.Rules) != 26 {
		t.Fatalf("rules: %d", len(s.Rules))
	}
	fails := 0
	for _, r := range s.Rules {
		if r.Status == "fail" {
			fails++
		}
		if r.Evidence == "" {
			t.Errorf("rule %s has empty evidence", r.ID)
		}
	}
	if fails != 20 {
		t.Fatalf("failing rules: %d, want 20", fails)
	}
}

func TestCLIServiceFilter(t *testing.T) {
	if _, err := runCLI(t, "-service", "payments", fixtureDir+"/good_service.json"); err != nil {
		t.Fatalf("filter hit: %v", err)
	}
	if _, err := runCLI(t, "-service", "absent", fixtureDir+"/good_service.json"); err == nil {
		t.Fatal("filter miss should error")
	}
}

func TestCLIMultiFileMerge(t *testing.T) {
	// Scoring one file or two halves must give the same answer: the good
	// fixture split into single-signal files still scores 100.
	out, err := runCLI(t, "-json", fixtureDir+"/good_service.json", fixtureDir+"/no_data_metrics_only.json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, `"service_name": "payments"`) || !strings.Contains(out, `"service_name": "metrics-only"`) {
		t.Fatalf("merged report missing services:\n%s", out)
	}
	if !strings.Contains(out, `"score": 61.43`) || !strings.Contains(out, `"score": 100`) {
		t.Fatalf("merged scores wrong:\n%s", out)
	}
}

func TestCLIUsageErrors(t *testing.T) {
	if _, err := runCLI(t); err == nil {
		t.Fatal("no args should error")
	}
	if _, err := runCLI(t, "/does/not/exist.json"); err == nil {
		t.Fatal("missing file should error")
	}
	if _, err := runCLI(t, fixtureDir+"/../../rulesync_test.go"); err == nil {
		t.Fatal("non-dump file should error")
	}
}
