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

import "testing"

// seedBindingTypes are the twelve types the WS-9 design §3.1 mandates. The test
// pins the vocabulary so a dropped or renamed seed type is a visible failure,
// not a silent gap that would strand a gated skill with no consumable credential.
var seedBindingTypes = []string{
	"github", "dynatrace", "terraform", "ansible", "proxmox-api",
	"linkedin-publish", "x-publish", "blog-publish",
	"gemini", "fal.ai", "heygen", "elevenlabs",
}

func TestLookupBindingKnownSeedTypes(t *testing.T) {
	for _, typ := range seedBindingTypes {
		b, ok := LookupBinding(typ)
		if !ok {
			t.Fatalf("LookupBinding(%q): want ok=true, got false — seed type missing", typ)
		}
		if b.Type != typ {
			t.Errorf("LookupBinding(%q).Type = %q, want %q", typ, b.Type, typ)
		}
		if b.DisplayName == "" {
			t.Errorf("LookupBinding(%q).DisplayName is empty", typ)
		}
		if b.DataKey == "" {
			t.Errorf("LookupBinding(%q).DataKey is empty", typ)
		}
		if !IsKnownType(typ) {
			t.Errorf("IsKnownType(%q): want true", typ)
		}
	}
}

// TestLookupBindingUnknownFailsClosed is the headline acceptance: an unknown
// type must fail CLOSED (ok=false, zero binding), never guess a name/key.
func TestLookupBindingUnknownFailsClosed(t *testing.T) {
	for _, typ := range []string{
		"", "unknown", "GitHub", "git-hub", "openai", "aws", "github ",
		"fal-ai", "dynatrace-api", "proxmox", "github-token",
	} {
		b, ok := LookupBinding(typ)
		if ok {
			t.Errorf("LookupBinding(%q): want ok=false (fail closed), got true", typ)
		}
		if b != (Binding{}) {
			t.Errorf("LookupBinding(%q): want zero Binding on miss, got %+v", typ, b)
		}
		if IsKnownType(typ) {
			t.Errorf("IsKnownType(%q): want false, got true", typ)
		}
	}
}

func TestBindingTypesSortedAndComplete(t *testing.T) {
	got := BindingTypes()
	if len(got) != len(seedBindingTypes) {
		t.Fatalf("BindingTypes() returned %d entries, want %d (%v)", len(got), len(seedBindingTypes), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("BindingTypes() not strictly sorted at %d: %q >= %q", i, got[i-1], got[i])
		}
	}
	set := map[string]bool{}
	for _, tp := range got {
		set[tp] = true
	}
	for _, want := range seedBindingTypes {
		if !set[want] {
			t.Errorf("BindingTypes() missing seed type %q", want)
		}
	}
}

// TestBindingTypesReturnsCopy proves a caller cannot mutate the registry through
// the returned slice (defence in depth — the table is the single source of truth).
func TestBindingTypesReturnsCopy(t *testing.T) {
	a := BindingTypes()
	if len(a) == 0 {
		t.Fatal("BindingTypes() empty")
	}
	a[0] = "MUTATED"
	if BindingTypes()[0] == "MUTATED" {
		t.Fatal("BindingTypes() leaked the backing array; a caller mutated the registry")
	}
}

func TestBindingConsumptionCarrierConsistency(t *testing.T) {
	for _, b := range Bindings() {
		switch b.Consumption {
		case ConsumptionToolchainEnv:
			if b.EnvVar == "" || b.HeaderName != "" {
				t.Errorf("type %q toolchain-env: EnvVar=%q HeaderName=%q", b.Type, b.EnvVar, b.HeaderName)
			}
		case ConsumptionMCPHeader:
			if b.HeaderName == "" || b.EnvVar != "" {
				t.Errorf("type %q mcp-header: EnvVar=%q HeaderName=%q", b.Type, b.EnvVar, b.HeaderName)
			}
		case ConsumptionFile:
			if b.EnvVar != "" || b.HeaderName != "" {
				t.Errorf("type %q file: EnvVar/HeaderName must be empty", b.Type)
			}
		default:
			t.Errorf("type %q has unknown Consumption %q", b.Type, b.Consumption)
		}
	}
}

func TestValidateBindingsPasses(t *testing.T) {
	if err := ValidateBindings(); err != nil {
		t.Fatalf("ValidateBindings() on the shipped registry: %v", err)
	}
}

func TestMaterializedSecretName(t *testing.T) {
	cases := []struct {
		project, typ, want string
		ok                 bool
	}{
		{"acme", "github", "proj-acme-cred-github", true},
		{"acme", "fal.ai", "proj-acme-cred-fal-ai", true},             // '.' → '-'
		{"My_Proj", "dynatrace", "proj-my-proj-cred-dynatrace", true}, // upper + '_' normalized
		{"p1", "x-publish", "proj-p1-cred-x-publish", true},
		{"acme", "unknown", "", false}, // fail closed — no name for an unmapped type
	}
	for _, c := range cases {
		got, ok := MaterializedSecretName(c.project, c.typ)
		if ok != c.ok {
			t.Errorf("MaterializedSecretName(%q,%q) ok=%v, want %v", c.project, c.typ, ok, c.ok)
		}
		if got != c.want {
			t.Errorf("MaterializedSecretName(%q,%q) = %q, want %q", c.project, c.typ, got, c.want)
		}
	}
}

// TestAnnotationKeyStable pins the annotation key S6 wires onto gated skills.
func TestAnnotationKeyStable(t *testing.T) {
	if AnnotationCredentialType != "ksquad.io/credential-type" {
		t.Fatalf("AnnotationCredentialType = %q, want ksquad.io/credential-type", AnnotationCredentialType)
	}
}

// TestSplitCredentialTypes pins the S6 annotation encoding (ISI-5819): a single
// bare token, an ANY-OF comma list, whitespace tolerance, and the empty cases —
// so the one reader S1/S5 share never drops a declared type nor invents one.
func TestSplitCredentialTypes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   ", nil},
		{"single", "github", []string{"github"}},
		{"single trimmed", "  github  ", []string{"github"}},
		{"anyof two", "gemini,fal.ai", []string{"gemini", "fal.ai"}},
		{"anyof spaced", "linkedin-publish, x-publish", []string{"linkedin-publish", "x-publish"}},
		{"blank members dropped", "gemini,,fal.ai,", []string{"gemini", "fal.ai"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitCredentialTypes(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("SplitCredentialTypes(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("SplitCredentialTypes(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestSplitCredentialTypesAllS6ValuesKnown is the end-to-end guard that every
// value the S6 skill annotations carry splits into tokens the registry maps —
// so a gated skill's declared ANY-OF list is fully consumable (no silent drop).
func TestSplitCredentialTypesAllS6ValuesKnown(t *testing.T) {
	// The exact annotation values authored on the WS-2/3/4/5 gated skills.
	s6Values := []string{
		"terraform", "ansible", "proxmox-api", // WS-2
		"gemini,fal.ai", "heygen,fal.ai", "elevenlabs", // WS-3
		"blog-publish",                    // WS-4
		"x-publish", "linkedin-publish", "linkedin-publish,x-publish", // WS-5
	}
	for _, v := range s6Values {
		for _, typ := range SplitCredentialTypes(v) {
			if !IsKnownType(typ) {
				t.Errorf("S6 annotation value %q yields unknown type %q (not in registry vocabulary)", v, typ)
			}
		}
	}
}
