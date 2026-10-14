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

package toolcred

import (
	"reflect"
	"testing"
)

func set(types ...string) map[string]bool {
	m := map[string]bool{}
	for _, t := range types {
		m[t] = true
	}
	return m
}

// TestEvaluateSkillGate pins the S5 go-live gate semantics (ISI-5818): any-of
// over a skill's declared types, fail-closed on unknowns, ungated when no
// gating type is declared.
func TestEvaluateSkillGate(t *testing.T) {
	cases := []struct {
		name       string
		annotation string
		satisfied  map[string]bool
		wantAdmit  bool
		wantMiss   []string
	}{
		{
			name:       "ungated: empty annotation admits",
			annotation: "",
			satisfied:  set(),
			wantAdmit:  true,
		},
		{
			name:       "single type satisfied admits",
			annotation: "github",
			satisfied:  set("github"),
			wantAdmit:  true,
		},
		{
			name:       "single type unsatisfied is withheld naming the type",
			annotation: "github",
			satisfied:  set("dynatrace"),
			wantAdmit:  false,
			wantMiss:   []string{"github"},
		},
		{
			name:       "any-of: one of two satisfied admits (gemini,fal.ai)",
			annotation: "gemini,fal.ai",
			satisfied:  set("fal.ai"),
			wantAdmit:  true,
		},
		{
			name:       "any-of: the other one satisfied admits",
			annotation: "gemini,fal.ai",
			satisfied:  set("gemini"),
			wantAdmit:  true,
		},
		{
			name:       "any-of: neither satisfied withholds naming both (sorted)",
			annotation: "gemini,fal.ai",
			satisfied:  set(),
			wantAdmit:  false,
			wantMiss:   []string{"fal.ai", "gemini"},
		},
		{
			name:       "any-of with spaces: trimmed, one satisfied admits",
			annotation: "linkedin-publish , x-publish",
			satisfied:  set("x-publish"),
			wantAdmit:  true,
		},
		{
			name:       "unknown-only type fails closed (withheld)",
			annotation: "bogus-provider",
			satisfied:  set("bogus-provider"), // cannot happen in reality; proves unknowns never admit
			wantAdmit:  false,
			wantMiss:   []string{"bogus-provider"},
		},
		{
			name:       "known+unknown: known satisfied admits via any-of",
			annotation: "github,bogus-provider",
			satisfied:  set("github"),
			wantAdmit:  true,
		},
		{
			name:       "known+unknown: none satisfied withholds naming both",
			annotation: "github,bogus-provider",
			satisfied:  set(),
			wantAdmit:  false,
			wantMiss:   []string{"bogus-provider", "github"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := EvaluateSkillGate(tc.annotation, tc.satisfied)
			if v.Admitted != tc.wantAdmit {
				t.Fatalf("Admitted = %v, want %v (verdict %+v)", v.Admitted, tc.wantAdmit, v)
			}
			if !reflect.DeepEqual(v.Missing, tc.wantMiss) {
				t.Fatalf("Missing = %v, want %v", v.Missing, tc.wantMiss)
			}
			if v.Admitted && len(v.Missing) != 0 {
				t.Fatalf("admitted verdict must carry no Missing, got %v", v.Missing)
			}
		})
	}
}

// TestEvaluateSkillGateUnknownNeverSatisfied proves the fail-closed invariant
// directly: because the real write path never materializes a Secret for an
// unknown type, an unknown token can never be in `satisfied`, so a skill
// declaring only unknown types is always withheld — exercised above with an
// impossible satisfied-set to pin the guarantee at the oracle layer too.
func TestEvaluateSkillGateUnknownIsGatingNotDropped(t *testing.T) {
	v := EvaluateSkillGate("bogus-provider", set())
	if len(v.Gating) != 1 || v.Gating[0] != "bogus-provider" {
		t.Fatalf("unknown token must be a hard gating need (fail closed), got Gating=%v", v.Gating)
	}
}

// TestEvaluateRunGate pins the whole-Run fold: admit iff every gated skill
// admits; MissingTypes is the deduped, sorted union across withheld skills.
func TestEvaluateRunGate(t *testing.T) {
	skills := []GatedSkill{
		{SourceName: "team-a/github-pr-review", Annotation: "github"},
		{SourceName: "team-a/image-gen", Annotation: "gemini,fal.ai"},
		{SourceName: "team-a/docs-skill", Annotation: ""}, // ungated
	}

	t.Run("all satisfied admits the run", func(t *testing.T) {
		d := EvaluateRunGate(skills, set("github", "gemini"))
		if !d.Admitted || len(d.WithheldSkills) != 0 || len(d.MissingTypes) != 0 {
			t.Fatalf("want admitted/empty, got %+v", d)
		}
	})

	t.Run("one withheld withholds the run and names missing", func(t *testing.T) {
		d := EvaluateRunGate(skills, set("gemini")) // github missing, image-gen ok via gemini
		if d.Admitted {
			t.Fatalf("one unsatisfied gated skill must withhold the run")
		}
		if !reflect.DeepEqual(d.WithheldSkills, []string{"team-a/github-pr-review"}) {
			t.Fatalf("WithheldSkills = %v", d.WithheldSkills)
		}
		if !reflect.DeepEqual(d.MissingTypes, []string{"github"}) {
			t.Fatalf("MissingTypes = %v, want [github]", d.MissingTypes)
		}
	})

	t.Run("missing types are deduped and sorted across skills", func(t *testing.T) {
		dup := []GatedSkill{
			{SourceName: "a/one", Annotation: "github"},
			{SourceName: "a/two", Annotation: "github,dynatrace"},
		}
		d := EvaluateRunGate(dup, set())
		if !reflect.DeepEqual(d.MissingTypes, []string{"dynatrace", "github"}) {
			t.Fatalf("MissingTypes = %v, want [dynatrace github] (deduped+sorted)", d.MissingTypes)
		}
	})
}
