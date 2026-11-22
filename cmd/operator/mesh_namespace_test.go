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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// meshNSClient builds a fake Team-reading client seeded with the given teams,
// each as (name, homeNamespace, uid, reconciledNamespace). A reconciled
// namespace of "" leaves Team.Status.Namespace empty (an unreconciled team).
func meshNSClient(t *testing.T, teams map[string]string) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add ksquad scheme: %v", err)
	}
	var objs []client.Object
	i := 0
	for name, reconciledNS := range teams {
		i++
		team := &ksquadv1.Team{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "ksquad-home",
				UID:       types.UID("uid-" + name),
			},
		}
		team.Status.Namespace = reconciledNS
		objs = append(objs, team)
		_ = i
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// ISI-7692 core: a mesh-flagged team resolves to its OWN reconciled squad
// namespace, NOT the shared fallback — the fix that lets a sympozium Run claim
// the sympozium standing pod instead of cold-booting past one parked in bmad's.
func TestResolveMeshPoolNamespace_PrefersTeamNamespaceOverFallback(t *testing.T) {
	c := meshNSClient(t, map[string]string{
		"sympozium-squad": "ksquad-team-sympozium-squad-1540d2f6",
		"bmad-squad":      "ksquad-team-bmad-squad-f6e8fc70",
	})
	// fallback is the bmad ns (the observed mis-placement source); the team
	// must still resolve to its OWN ns.
	ns, fromTeam, err := resolveMeshPoolNamespace(context.Background(), c, "sympozium-squad", "ksquad-team-bmad-squad-f6e8fc70")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ns != "ksquad-team-sympozium-squad-1540d2f6" {
		t.Fatalf("namespace = %q; want the sympozium team ns, not the bmad fallback", ns)
	}
	if !fromTeam {
		t.Fatalf("fromTeamCR = false; want true (resolved off the Team CR)")
	}
}

// Each flagged team resolves to its distinct namespace from one shared client —
// pins that the single-fallback bug (all teams in one ns) cannot recur.
func TestResolveMeshPoolNamespace_PerTeamDistinct(t *testing.T) {
	c := meshNSClient(t, map[string]string{
		"sympozium-squad": "ksquad-team-sympozium-squad-1540d2f6",
		"bmad-squad":      "ksquad-team-bmad-squad-f6e8fc70",
	})
	got := map[string]string{}
	for _, team := range []string{"sympozium-squad", "bmad-squad"} {
		ns, _, err := resolveMeshPoolNamespace(context.Background(), c, team, "")
		if err != nil {
			t.Fatalf("team %q: unexpected error: %v", team, err)
		}
		got[team] = ns
	}
	if got["sympozium-squad"] == got["bmad-squad"] {
		t.Fatalf("both teams resolved to the same ns %q; per-team isolation lost", got["sympozium-squad"])
	}
}

// An unreconciled team (empty Status.Namespace) falls back to the provided
// fallback and reports fromTeamCR=false so the caller can log the degrade.
func TestResolveMeshPoolNamespace_FallbackWhenUnreconciled(t *testing.T) {
	c := meshNSClient(t, map[string]string{"sympozium-squad": ""})
	ns, fromTeam, err := resolveMeshPoolNamespace(context.Background(), c, "sympozium-squad", "fallback-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ns != "fallback-ns" {
		t.Fatalf("namespace = %q; want fallback-ns", ns)
	}
	if fromTeam {
		t.Fatalf("fromTeamCR = true; want false (fell back)")
	}
}

// An unresolved team with NO fallback errors — the caller must skip the boot
// rather than guess a namespace (guessing one is the ISI-7692 defect).
func TestResolveMeshPoolNamespace_ErrorsWhenUnresolvedAndNoFallback(t *testing.T) {
	c := meshNSClient(t, map[string]string{"other-team": "ksquad-team-other"})
	_, _, err := resolveMeshPoolNamespace(context.Background(), c, "sympozium-squad", "")
	if err == nil {
		t.Fatalf("expected an error for an unresolved team with no fallback, got nil")
	}
}
