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
	"sort"
	"testing"
)

// TestProjectSatisfiedTypesScopesByName: the satisfied set is built from the
// project-encoded Secret NAME, so a present probe keyed on the WRONG project's
// name never marks the type satisfied — the ISI-5849 no-sibling-over-admit
// property. A present map scoped to projectA's names leaves projectB unsatisfied.
func TestProjectSatisfiedTypesScopesByName(t *testing.T) {
	// Only projectA's gemini Secret exists.
	nameA, _ := MaterializedSecretName("proj-a", "gemini")
	present := func(name string) bool { return name == nameA }

	satA := ProjectSatisfiedTypes("proj-a", []string{"gemini"}, present)
	if !satA["gemini"] {
		t.Fatalf("proj-a gemini should be satisfied by its own Secret %q", nameA)
	}

	// proj-b probes proj-b-cred-gemini, which the present probe does NOT have,
	// so a sibling project's materialized Secret must not admit proj-b.
	satB := ProjectSatisfiedTypes("proj-b", []string{"gemini"}, present)
	if satB["gemini"] {
		t.Fatalf("proj-b gemini must NOT be satisfied by proj-a's Secret (sibling over-admit)")
	}
}

// TestProjectSatisfiedTypesFailsClosedUnknown: an unknown type has no
// materializable name, so it is skipped — never seeded into the satisfied set
// even if a (malformed) present probe would answer true.
func TestProjectSatisfiedTypesFailsClosedUnknown(t *testing.T) {
	sat := ProjectSatisfiedTypes("proj-a", []string{"mystery", "gemini"}, func(string) bool { return true })
	if sat["mystery"] {
		t.Fatalf("unknown type must never be satisfied (fail closed)")
	}
	if !sat["gemini"] {
		t.Fatalf("known type with a present Secret should be satisfied")
	}
}

// TestProjectSatisfiedTypesNilProbe: a nil present probe yields an empty set
// (fail closed) rather than panicking.
func TestProjectSatisfiedTypesNilProbe(t *testing.T) {
	if got := ProjectSatisfiedTypes("proj-a", []string{"gemini"}, nil); len(got) != 0 {
		t.Fatalf("nil probe = %v, want empty satisfied set", got)
	}
}

// TestProjectSatisfiedTypesFeedsGate: the satisfied set threads into
// EvaluateRunGate end to end — an unsatisfied any-of skill is withheld with its
// declared types named; materializing one admits it.
func TestProjectSatisfiedTypesFeedsGate(t *testing.T) {
	skills := []GatedSkill{{SourceName: "default/image-gen", Annotation: "gemini,fal.ai"}}

	// Nothing materialized ⇒ withheld, both types named.
	none := ProjectSatisfiedTypes("proj-a", []string{"gemini", "fal.ai"}, func(string) bool { return false })
	d := EvaluateRunGate(skills, none)
	if d.Admitted {
		t.Fatalf("expected withheld with no materialized types")
	}
	want := []string{"fal.ai", "gemini"}
	got := append([]string(nil), d.MissingTypes...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MissingTypes = %v, want %v", got, want)
	}

	// Materialize fal.ai only ⇒ any-of admits.
	falName, _ := MaterializedSecretName("proj-a", "fal.ai")
	one := ProjectSatisfiedTypes("proj-a", []string{"gemini", "fal.ai"}, func(n string) bool { return n == falName })
	if !EvaluateRunGate(skills, one).Admitted {
		t.Fatalf("expected admitted once one any-of provider is materialized")
	}
}
