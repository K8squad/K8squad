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
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	kubepool "github.com/K8squad/K8squad/pkg/warmpool"
)

// team seeds a Team CR with a reconciled squad namespace (empty ns ⇒
// unreconciled, Status.Namespace stays empty). homeNS disambiguates the home
// namespace when two teams share a name.
func team(name, homeNS, reconciledNS string) *ksquadv1.Team {
	t := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: homeNS, UID: types.UID("uid-" + homeNS + "-" + name)},
	}
	t.Status.Namespace = reconciledNS
	return t
}

func meshNSClient(t *testing.T, teams ...*ksquadv1.Team) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add ksquad scheme: %v", err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, tm := range teams {
		b = b.WithObjects(tm)
	}
	return b.Build()
}

func specTable(teams []string, role string) map[string]kubepool.MeshPoolSpec {
	m := map[string]kubepool.MeshPoolSpec{}
	for _, tm := range teams {
		m[kubepool.MeshPoolPodName(tm, role)] = kubepool.MeshPoolSpec{Team: tm, Role: role, Image: "img"}
	}
	return m
}

// recordingBooter captures the specs it was asked to boot (keyed by team) and
// can be told to fail a given team's boot.
type recordingBooter struct {
	booted map[string]kubepool.MeshPoolSpec
	fail   map[string]bool
}

func newRecordingBooter() *recordingBooter {
	return &recordingBooter{booted: map[string]kubepool.MeshPoolSpec{}, fail: map[string]bool{}}
}

func (r *recordingBooter) BootMeshPool(_ context.Context, spec kubepool.MeshPoolSpec) error {
	if r.fail[spec.Team] {
		return errors.New("boom")
	}
	r.booted[spec.Team] = spec
	return nil
}

// recordingCleaner captures mis-located cleanup calls as "team@ns".
type recordingCleaner struct{ calls []string }

func (c *recordingCleaner) DeleteMislocatedMeshPool(_ context.Context, ns, team, _ string) error {
	c.calls = append(c.calls, team+"@"+ns)
	return nil
}

// ---- resolveTeamNamespaces ----

func TestResolveTeamNamespaces_SnapshotAndSkipsUnreconciled(t *testing.T) {
	c := meshNSClient(t,
		team("sympozium-squad", "ksquad-home", "ksquad-team-sympozium-squad-1540d2f6"),
		team("bmad-squad", "ksquad-home", "ksquad-team-bmad-squad-f6e8fc70"),
		team("not-ready", "ksquad-home", ""), // unreconciled → absent
	)
	got, err := resolveTeamNamespaces(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["sympozium-squad"] != "ksquad-team-sympozium-squad-1540d2f6" {
		t.Fatalf("sympozium resolved to %q", got["sympozium-squad"])
	}
	if got["bmad-squad"] != "ksquad-team-bmad-squad-f6e8fc70" {
		t.Fatalf("bmad resolved to %q", got["bmad-squad"])
	}
	if _, ok := got["not-ready"]; ok {
		t.Fatalf("unreconciled team must be absent, got %q", got["not-ready"])
	}
}

// A name that resolves to two different namespaces is ambiguous and dropped —
// never reported as authoritative (which the unordered informer index would
// otherwise do non-deterministically).
func TestResolveTeamNamespaces_AmbiguousNameDropped(t *testing.T) {
	c := meshNSClient(t,
		team("dup", "home-a", "ns-a"),
		team("dup", "home-b", "ns-b"),
		team("solo", "home-a", "ns-solo"),
	)
	got, err := resolveTeamNamespaces(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got["dup"]; ok {
		t.Fatalf("ambiguous name must be dropped, got %q", got["dup"])
	}
	if got["solo"] != "ns-solo" {
		t.Fatalf("solo resolved to %q", got["solo"])
	}
}

// ---- bootMeshPools: the apply-the-resolution logic (ISI-7692 finding 5) ----

// The resolved namespace actually reaches the booted spec — deleting
// spec.Namespace=ns in the code would turn this red.
func TestBootMeshPools_BootsIntoResolvedTeamNamespace(t *testing.T) {
	c := meshNSClient(t, team("sympozium-squad", "h", "ksquad-team-sympozium-squad-1540d2f6"))
	booter := newRecordingBooter()
	cleaner := &recordingCleaner{}
	teams := []string{"sympozium-squad"}
	remaining := bootMeshPools(context.Background(), c, booter, cleaner, specTable(teams, "coder"), teams, "coder", "", false, logr.Discard())
	if len(remaining) != 0 {
		t.Fatalf("remaining = %v; want none", remaining)
	}
	if booter.booted["sympozium-squad"].Namespace != "ksquad-team-sympozium-squad-1540d2f6" {
		t.Fatalf("booted ns = %q; want the sympozium team ns", booter.booted["sympozium-squad"].Namespace)
	}
}

// Each flagged team boots into its OWN namespace from one shared client — the
// single-shared-ns bug cannot recur.
func TestBootMeshPools_PerTeamDistinct(t *testing.T) {
	c := meshNSClient(t,
		team("sympozium-squad", "h", "ns-sympozium"),
		team("bmad-squad", "h", "ns-bmad"),
	)
	booter := newRecordingBooter()
	teams := []string{"sympozium-squad", "bmad-squad"}
	bootMeshPools(context.Background(), c, booter, &recordingCleaner{}, specTable(teams, "coder"), teams, "coder", "", false, logr.Discard())
	if booter.booted["sympozium-squad"].Namespace == booter.booted["bmad-squad"].Namespace {
		t.Fatalf("both teams booted into the same ns %q", booter.booted["sympozium-squad"].Namespace)
	}
}

// Multi-team + unresolved team ⇒ NOT booted (no fallback guess), returned for
// retry. Boot into the fallback ns is the defect.
func TestBootMeshPools_MultiTeamUnresolvedSkipsNoFallback(t *testing.T) {
	c := meshNSClient(t,
		team("ready", "h", "ns-ready"),
		team("pending", "h", ""), // unreconciled
	)
	booter := newRecordingBooter()
	teams := []string{"ready", "pending"}
	// allowFallback=false (multi-team), fallbackNS set to the bmad ns.
	remaining := bootMeshPools(context.Background(), c, booter, &recordingCleaner{}, specTable(teams, "coder"), teams, "coder", "ns-bmad", false, logr.Discard())
	if _, booted := booter.booted["pending"]; booted {
		t.Fatalf("unresolved team must NOT boot into the fallback ns")
	}
	if len(remaining) != 1 || remaining[0] != "pending" {
		t.Fatalf("remaining = %v; want [pending] for retry", remaining)
	}
	if booter.booted["ready"].Namespace != "ns-ready" {
		t.Fatalf("resolved team must still boot into its own ns")
	}
}

// Single-team + unresolved + fallback set ⇒ honored (the one case where the
// fallback is safe), booted into the fallback, not returned for retry.
func TestBootMeshPools_SingleTeamFallbackHonored(t *testing.T) {
	c := meshNSClient(t, team("solo", "h", "")) // unreconciled
	booter := newRecordingBooter()
	teams := []string{"solo"}
	remaining := bootMeshPools(context.Background(), c, booter, &recordingCleaner{}, specTable(teams, "coder"), teams, "coder", "fallback-ns", true, logr.Discard())
	if booter.booted["solo"].Namespace != "fallback-ns" {
		t.Fatalf("single-team fallback not honored: ns=%q", booter.booted["solo"].Namespace)
	}
	if len(remaining) != 0 {
		t.Fatalf("fallback boot should be done, remaining=%v", remaining)
	}
}

// The mis-located orphan in the fallback ns is cleaned up when the resolved ns
// differs (ISI-7692 finding 2).
func TestBootMeshPools_CleansUpMislocatedOrphan(t *testing.T) {
	c := meshNSClient(t, team("sympozium-squad", "h", "ns-sympozium"))
	cleaner := &recordingCleaner{}
	teams := []string{"sympozium-squad"}
	bootMeshPools(context.Background(), c, newRecordingBooter(), cleaner, specTable(teams, "coder"), teams, "coder", "ns-bmad", false, logr.Discard())
	if len(cleaner.calls) != 1 || cleaner.calls[0] != "sympozium-squad@ns-bmad" {
		t.Fatalf("expected one cleanup of the orphan in the fallback ns, got %v", cleaner.calls)
	}
}

// No cleanup when the resolved ns IS the fallback (nothing mis-located).
func TestBootMeshPools_NoCleanupWhenResolvedEqualsFallback(t *testing.T) {
	c := meshNSClient(t, team("solo", "h", "ns-x"))
	cleaner := &recordingCleaner{}
	teams := []string{"solo"}
	bootMeshPools(context.Background(), c, newRecordingBooter(), cleaner, specTable(teams, "coder"), teams, "coder", "ns-x", true, logr.Discard())
	if len(cleaner.calls) != 0 {
		t.Fatalf("no cleanup expected when resolved ns == fallback, got %v", cleaner.calls)
	}
}

// A boot failure returns the team for retry (no stale state, no crash).
func TestBootMeshPools_BootFailureRetried(t *testing.T) {
	c := meshNSClient(t, team("sympozium-squad", "h", "ns-sympozium"))
	booter := newRecordingBooter()
	booter.fail["sympozium-squad"] = true
	teams := []string{"sympozium-squad"}
	remaining := bootMeshPools(context.Background(), c, booter, &recordingCleaner{}, specTable(teams, "coder"), teams, "coder", "", false, logr.Discard())
	if len(remaining) != 1 || remaining[0] != "sympozium-squad" {
		t.Fatalf("boot failure must be retried, remaining=%v", remaining)
	}
}

// A List error boots nothing and retries every team — never fans them into the
// fallback (ISI-7692 inline on mesh_namespace.go: a List failure is not a
// licence to guess).
func TestBootMeshPools_ListErrorBootsNothing(t *testing.T) {
	teams := []string{"a", "b"}
	remaining := bootMeshPools(context.Background(), errReader{}, newRecordingBooter(), &recordingCleaner{}, specTable(teams, "coder"), teams, "coder", "fallback-ns", true, logr.Discard())
	sort.Strings(remaining)
	if len(remaining) != 2 || remaining[0] != "a" || remaining[1] != "b" {
		t.Fatalf("a List error must return all teams for retry, got %v", remaining)
	}
}

// errReader fails List — the cache-not-ready / RBAC-denied arm.
type errReader struct{ client.Reader }

func (errReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("cache not ready")
}

// ---- race safety (ISI-7692 finding 1) ----

// The boot leg and the recreate leg share the spec table with concurrent READS
// only — no write-back. Run both legs hard in parallel; `go test -race` would
// flag a map read/write race if the fix regressed to mutating the table.
func TestMeshSpecTable_ConcurrentReadersNoRace(t *testing.T) {
	c := meshNSClient(t, team("sympozium-squad", "h", "ns-sympozium"))
	teams := []string{"sympozium-squad"}
	specs := specTable(teams, "coder")
	rec := meshPodRecreator{provisioner: nil, specs: specs, reader: c, fallbackNS: "", allowFallback: false}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			bootMeshPools(context.Background(), c, newRecordingBooter(), &recordingCleaner{}, specs, teams, "coder", "", false, logr.Discard())
		}()
		go func() {
			defer wg.Done()
			// Read the shared table the way RecreatePod does (stop before the
			// nil-provisioner RecreateMeshPool call).
			_ = rec.specs[kubepool.MeshPoolPodName("sympozium-squad", "coder")]
			_, _ = resolveTeamNamespaces(context.Background(), c)
		}()
	}
	wg.Wait()
}
