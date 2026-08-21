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

package team

import (
	"regexp"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func newTeam(name, uid string) *api.Team {
	return &api.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(uid),
		},
	}
}

// TestNamespaceNameDeterministic1to1 (AC1): same Team → same namespace
// (deterministic), distinct Teams (even same name, different UID) → distinct
// namespaces (1:1, collision-safe), and the result is always DNS-1123-safe
// and ≤63 chars even for hostile input.
func TestNamespaceNameDeterministic1to1(t *testing.T) {
	a := newTeam("Alpha.Squad_1", "uid-a")
	first := NamespaceNameFor(a)
	if first != NamespaceNameFor(a) {
		t.Fatalf("namespace derivation not deterministic: %s vs %s", first, NamespaceNameFor(a))
	}

	b := newTeam("Alpha.Squad_1", "uid-b") // same name, different Team
	if first == NamespaceNameFor(b) {
		t.Fatalf("two distinct Teams resolved to the same namespace %q (AC1 1:1 violation)", first)
	}

	cases := []struct{ name, uid string }{
		{"normal-squad", "u1"},
		{"UPPER.Case_and_Symbols!", "u2"},
		{string(make([]byte, 0)), "u3"}, // empty name
		{"a-very-long-team-name-that-exceeds-the-dns-label-limit-of-sixty-three-characters-by-far", "u4"},
		{"...---...", "u5"},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		team := newTeam(tc.name, tc.uid)
		ns := NamespaceNameFor(team)
		if len(ns) > maxNameLength {
			t.Errorf("namespace %q exceeds 63 chars (name=%q)", ns, tc.name)
		}
		if !dns1123.MatchString(ns) {
			t.Errorf("namespace %q is not DNS-1123 safe (name=%q)", ns, tc.name)
		}
		if prev, dup := seen[ns]; dup {
			t.Errorf("namespace %q derived for both %q and %q", ns, prev, tc.name)
		}
		seen[ns] = tc.name
	}
}

// TestNamespaceNameNeverSystem (AC7): no derivation lands on a reserved
// system namespace.
func TestNamespaceNameNeverSystem(t *testing.T) {
	for _, name := range []string{"system", "kube-system", "ksquad-system", "default"} {
		ns := NamespaceNameFor(newTeam(name, "uid-"+name))
		if IsReservedNamespace(ns) {
			t.Errorf("Team %q derived reserved namespace %q", name, ns)
		}
	}
	for _, reserved := range []string{"ksquad-system", "kube-system", "kube-public", "kube-node-lease", "default"} {
		if !IsReservedNamespace(reserved) {
			t.Errorf("%q should be reserved", reserved)
		}
		if IsReservedNamespace("ksquad-team-alpha-abcdef12") {
			t.Error("a derived squad namespace must never be classified reserved")
		}
	}
}

// TestNormalizeName: invalid chars collapse, output trimmed and never empty.
func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Alpha.Squad_1":  "alpha-squad-1",
		"  spaces  ":     "spaces",
		"...":            "team",
		"Mixed---Dashes": "mixed-dashes",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
