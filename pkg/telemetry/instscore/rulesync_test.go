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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// rulesDir points at the vendored rule markdown relative to this package.
const rulesDir = "../../../third_party/instrumentation-score/rules"

// Field patterns for the vendored rule files' bold-marked header fields.
var (
	ruleIDPat    = regexp.MustCompile(`(?m)^\*\*Rule ID:\*\*\s+(.+?)\s*$`)
	ruleTargetPt = regexp.MustCompile(`(?m)^\*\*Target:\*\*\s+(.+?)\s*$`)
	ruleImpactPt = regexp.MustCompile(`(?m)^\*\*Impact:\*\*\s+(.+?)\s*$`)
)

// vendoredRule is one rule markdown file parsed from the vendored tree.
type vendoredRule struct {
	ID     string
	Target string
	Impact string
}

// parseVendoredRule extracts the Rule ID, Target, and Impact header fields
// from a vendored rule markdown file.
func parseVendoredRule(t *testing.T, path string) vendoredRule {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	get := func(pat *regexp.Regexp, field string) string {
		m := pat.FindSubmatch(body)
		if m == nil {
			t.Fatalf("%s: missing %q field", path, field)
		}
		return string(m[1])
	}
	return vendoredRule{
		ID:     get(ruleIDPat, "Rule ID"),
		Target: get(ruleTargetPt, "Target"),
		Impact: get(ruleImpactPt, "Impact"),
	}
}

// TestRulesyncRegistryMatchesVendoredRules is the rulesync test: it asserts
// the Go registry in rules.go matches the vendored rule markdown (count,
// ids, targets, impacts), so upstream drift — or an un-synced registry edit —
// is a visible test failure, not a silent score change.
func TestRulesyncRegistryMatchesVendoredRules(t *testing.T) {
	entries, err := os.ReadDir(rulesDir)
	if err != nil {
		t.Fatalf("read vendored rules dir %s: %v (did you forget to vendor third_party/instrumentation-score?)", rulesDir, err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == "_template.md" || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	if len(files) == 0 {
		t.Fatalf("no rule markdown found under %s", rulesDir)
	}
	if len(files) != len(Rules) {
		t.Errorf("vendored rule count %d != registry count %d — re-sync the registry in rules.go with the vendored tree (see third_party/instrumentation-score/README.md)", len(files), len(Rules))
	}

	registry := make(map[string]Rule, len(Rules))
	for _, r := range Rules {
		if _, dup := registry[r.ID]; dup {
			t.Errorf("registry lists rule %s twice", r.ID)
		}
		registry[r.ID] = r
	}

	for _, name := range files {
		v := parseVendoredRule(t, filepath.Join(rulesDir, name))
		r, ok := registry[v.ID]
		if !ok {
			t.Errorf("%s: vendored rule %s is missing from the registry in rules.go", name, v.ID)
			continue
		}
		delete(registry, v.ID)

		if base := v.ID + ".md"; name != base {
			t.Errorf("%s: file name does not match its Rule ID %s", name, v.ID)
		}
		if got, want := parseTargets(v.Target), r.Target; !equalTargets(got, want) {
			t.Errorf("%s: registry target %v != vendored target %v", v.ID, want, got)
		}
		if got, want := Impact(v.Impact), r.Impact; got != want {
			t.Errorf("%s: registry impact %q != vendored impact %q", v.ID, want, got)
		}
		if w := r.Impact.Weight(); w == 0 {
			t.Errorf("%s: registry impact %q has no specification weight", v.ID, r.Impact)
		}
	}
	for id := range registry {
		t.Errorf("registry rule %s has no vendored markdown under %s", id, rulesDir)
	}
}

func equalTargets(a, b []Target) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRulesSortedByID pins the registry's sorted-by-ID ordering, which the
// console and API renderings rely on for stable output.
func TestRulesSortedByID(t *testing.T) {
	for i := 1; i < len(Rules); i++ {
		if Rules[i-1].ID >= Rules[i].ID {
			t.Errorf("registry not sorted by ID: %s >= %s", Rules[i-1].ID, Rules[i].ID)
		}
	}
}

// TestByIDLooksUpEveryRule keeps the ByID helper honest against the registry.
func TestByIDLooksUpEveryRule(t *testing.T) {
	for _, r := range Rules {
		got, ok := ByID(r.ID)
		if !ok {
			t.Errorf("ByID(%s) not found", r.ID)
			continue
		}
		if got.ID != r.ID || got.Impact != r.Impact || !equalTargets(got.Target, r.Target) {
			t.Errorf("ByID(%s) returned mismatched entry %+v", r.ID, got)
		}
	}
	if _, ok := ByID("NOPE-000"); ok {
		t.Error("ByID returned true for an unknown rule")
	}
}

// Field patterns for the guidance sync (phase 4, ISI-7750). The vendored tree
// spells the bold header two ways — `**Criteria:**` and (MET-006 only)
// `**Criteria**:` — so the pattern accepts the colon inside or outside the
// bold markers. Values run to the next bold header (multi-paragraph Criteria
// such as SPA-006's sensitive-format list stay whole), matching the generator
// that pinned guidance.go.
var (
	ruleDescPat  = regexp.MustCompile(`(?s)\*\*Description:?\*\*:?\s*(.*?)\n\n\*\*`)
	ruleCritPat  = regexp.MustCompile(`(?s)\*\*Criteria:?\*\*:?\s*(.*?)\n\n\*\*`)
	guidanceSync = normalizeVendoredText
)

// TestRulesyncGuidanceMatchesVendoredRules extends the rulesync contract to
// the phase-4 read surface's pinned prose: every vendored rule's Description
// and Criteria must appear in guidance.go verbatim under the documented
// normalization (whitespace collapsed, "\=" unescaped), and the maps must
// carry no extra or missing rule IDs. A failure means the vendored pin moved
// (or guidance.go was hand-edited) — re-generate, never patch by hand.
func TestRulesyncGuidanceMatchesVendoredRules(t *testing.T) {
	entries, err := os.ReadDir(rulesDir)
	if err != nil {
		t.Fatalf("read vendored rules dir %s: %v", rulesDir, err)
	}

	seen := make(map[string]bool, len(ruleDescriptions))
	for id := range ruleDescriptions {
		if _, ok := ruleGuidance[id]; !ok {
			t.Errorf("ruleDescriptions lists %s but ruleGuidance does not — the maps must cover the same rule set", id)
		}
		seen[id] = true
	}
	for id := range ruleGuidance {
		if !seen[id] {
			t.Errorf("ruleGuidance lists %s but ruleDescriptions does not — the maps must cover the same rule set", id)
		}
	}

	for _, e := range entries {
		if e.IsDir() || e.Name() == "_template.md" || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(rulesDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		id := string(ruleIDPat.FindSubmatch(body)[1])

		dm := ruleDescPat.FindSubmatch(body)
		if dm == nil {
			t.Errorf("%s: vendored rule has no Description field", e.Name())
			continue
		}
		if got := guidanceSync(string(dm[1])); got != ruleDescriptions[id] {
			t.Errorf("%s: pinned Description drift:\n vendored: %q\n pinned:  %q", e.Name(), got, ruleDescriptions[id])
		}

		cm := ruleCritPat.FindSubmatch(body)
		if cm == nil {
			t.Errorf("%s: vendored rule has no Criteria field", e.Name())
			continue
		}
		if got := guidanceSync(string(cm[1])); got != ruleGuidance[id] {
			t.Errorf("%s: pinned guidance (Criteria) drift:\n vendored: %q\n pinned:  %q", e.Name(), got, ruleGuidance[id])
		}
		seen[id] = false
	}
	for id, left := range seen {
		if left {
			t.Errorf("guidance maps list %s but no vendored markdown exists under %s", id, rulesDir)
		}
	}
}

// TestGuidanceAndDescriptionLookups keeps the accessors honest for every
// registry rule — the read surface assumes both resolve for all 26 IDs.
func TestGuidanceAndDescriptionLookups(t *testing.T) {
	for _, r := range Rules {
		if d, ok := Description(r.ID); !ok || d == "" {
			t.Errorf("Description(%s) missing or empty", r.ID)
		}
		if g, ok := FailedGuidance(r.ID); !ok || g == "" {
			t.Errorf("FailedGuidance(%s) missing or empty", r.ID)
		}
	}
	if _, ok := Description("NOPE-000"); ok {
		t.Error("Description returned true for an unknown rule")
	}
	if _, ok := FailedGuidance("NOPE-000"); ok {
		t.Error("FailedGuidance returned true for an unknown rule")
	}
}
