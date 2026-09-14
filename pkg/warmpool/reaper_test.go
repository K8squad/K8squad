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

package warmpool_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/K8squad/K8squad/pkg/warmpool"
)

// ISI-4348-S2 (ADR-0020 §2.3) — the restart-safe reaper decision. A run-owned
// sandbox pod (task-io Secret present) is reaped ONLY when its a2a follow has
// durably settled AND no live follow exists in this process; every other case
// keeps the pod (protecting an agent still working across a restart, F3). These
// pin the ticket's unit acceptance directly.

// fakeSettle is the SettlementReader test double: settled[runID] is the durable
// marker state; err forces a reader failure (which must fail closed).
type fakeSettle struct {
	settled map[string]bool
	err     error
}

func (f fakeSettle) Settled(_ context.Context, runID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.settled[runID], nil
}

// fakeFollow is the FollowOracle test double: live[runID] reports an in-process
// follow goroutine.
type fakeFollow struct{ live map[string]bool }

func (f fakeFollow) IsFollowing(runID string) bool { return f.live[runID] }

// seedRunOwnedPod boots a run-owned sandbox: a provably-managed Ready pod, the
// AnnRunID stamp the Bind path writes (ADR-0020 §2.3), and the task-io Secret
// that marks it run-owned. runID == "" seeds an UNSTAMPED pod (the fail-closed
// case).
func seedRunOwnedPod(t *testing.T, ctx context.Context, c client.Client, p *warmpool.KubeProvisioner, name, runID string) {
	t.Helper()
	bootWarmPod(t, ctx, c, p, gvisorKey, name, 3, true)
	if runID != "" {
		pod := &corev1.Pod{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, pod); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[warmpool.AnnRunID] = runID
		if err := c.Update(ctx, pod); err != nil {
			t.Fatalf("stamp run-id on %s: %v", name, err)
		}
	}
	seedRunOwnedSecret(t, ctx, c, "default", name)
}

func TestReaperReapsSettledNotFollowing(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "run-123")

	pool := warmpool.NewPool(newFakeProvisioner())
	settle := fakeSettle{settled: map[string]bool{"run-123": true}}
	follow := fakeFollow{live: map[string]bool{}} // not following (post-restart)

	report, err := warmpool.AdoptOrReap(ctx, c, pool, settle, follow, gvisorKey)
	if err != nil {
		t.Fatalf("adopt-or-reap: %v", err)
	}
	if report.Reaped != 1 {
		t.Errorf("reaped = %d, want 1 (settled + not following ⇒ reap)", report.Reaped)
	}
	if report.LeftRunOwned != 0 {
		t.Errorf("leftRunOwned = %d, want 0", report.LeftRunOwned)
	}
	assertPodGone(t, ctx, c, "default", "run-owned")
}

func TestReaperKeepsUnsettled(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "run-123")

	pool := warmpool.NewPool(newFakeProvisioner())
	settle := fakeSettle{settled: map[string]bool{}} // NOT settled — agent may still work
	follow := fakeFollow{live: map[string]bool{}}

	report, err := warmpool.AdoptOrReap(ctx, c, pool, settle, follow, gvisorKey)
	if err != nil {
		t.Fatalf("adopt-or-reap: %v", err)
	}
	if report.Reaped != 0 {
		t.Errorf("reaped = %d, want 0 (not settled ⇒ keep, F3)", report.Reaped)
	}
	if report.LeftRunOwned != 1 {
		t.Errorf("leftRunOwned = %d, want 1", report.LeftRunOwned)
	}
	assertPodExists(t, ctx, c, "default", "run-owned")
}

func TestReaperKeepsFollowing(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "run-123")

	pool := warmpool.NewPool(newFakeProvisioner())
	// Settled AND still following in-process — steady state, not leaked.
	// OnDone's own Release owns teardown here; the reaper must NOT race it.
	settle := fakeSettle{settled: map[string]bool{"run-123": true}}
	follow := fakeFollow{live: map[string]bool{"run-123": true}}

	report, err := warmpool.AdoptOrReap(ctx, c, pool, settle, follow, gvisorKey)
	if err != nil {
		t.Fatalf("adopt-or-reap: %v", err)
	}
	if report.Reaped != 0 {
		t.Errorf("reaped = %d, want 0 (live follow ⇒ keep)", report.Reaped)
	}
	if report.LeftRunOwned != 1 {
		t.Errorf("leftRunOwned = %d, want 1", report.LeftRunOwned)
	}
	assertPodExists(t, ctx, c, "default", "run-owned")
}

// An unstamped run-owned pod (no AnnRunID — a pre-ADR-0020 pod, or a stamp write
// that raced/failed) cannot be checked for settlement, so it fails CLOSED: kept,
// never reaped, even when a settled marker exists for some other run.
func TestReaperKeepsUnstampedPod(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "") // no run-id stamp

	pool := warmpool.NewPool(newFakeProvisioner())
	settle := fakeSettle{settled: map[string]bool{"run-123": true}}
	follow := fakeFollow{live: map[string]bool{}}

	report, err := warmpool.AdoptOrReap(ctx, c, pool, settle, follow, gvisorKey)
	if err != nil {
		t.Fatalf("adopt-or-reap: %v", err)
	}
	if report.Reaped != 0 || report.LeftRunOwned != 1 {
		t.Errorf("report = %+v, want 0 reaped / 1 leftRunOwned (no run-id ⇒ fail closed)", report)
	}
	assertPodExists(t, ctx, c, "default", "run-owned")
}

// A settlement-reader error fails closed: the pod is kept (never reaped on a
// maybe) AND the error is surfaced so the operator logs it.
func TestReaperReaderErrorKeepsAndReports(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "run-123")

	pool := warmpool.NewPool(newFakeProvisioner())
	settle := fakeSettle{err: errors.New("db down")}
	follow := fakeFollow{live: map[string]bool{}}

	report, err := warmpool.AdoptOrReap(ctx, c, pool, settle, follow, gvisorKey)
	if err == nil {
		t.Fatal("want a surfaced reader error, got nil")
	}
	if report.Reaped != 0 {
		t.Errorf("reaped = %d, want 0 (reader error ⇒ never reap)", report.Reaped)
	}
	assertPodExists(t, ctx, c, "default", "run-owned")
}

// A nil reader/oracle (reaper not wired) leaves every run-owned pod to
// run-drive — no reap, no panic. Guards the operator's fail-safe default.
func TestReaperNilDepsLeavesRunOwned(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "run-123")

	pool := warmpool.NewPool(newFakeProvisioner())
	report, err := warmpool.AdoptOrReap(ctx, c, pool, nil, nil, gvisorKey)
	if err != nil {
		t.Fatalf("adopt-or-reap: %v", err)
	}
	if report.Reaped != 0 || report.LeftRunOwned != 1 {
		t.Errorf("report = %+v, want 0 reaped / 1 leftRunOwned (nil deps ⇒ keep)", report)
	}
	assertPodExists(t, ctx, c, "default", "run-owned")
}

// SweepSettledRunOwned (the warm-tick backstop) reaps a settled+not-following
// run-owned pod and IGNORES warm (no-Secret) pods entirely — it never adopts,
// scales, or reaps warmth (that stays AdoptOrReap's job).
func TestSweepBackstopReapsSettledIgnoresWarm(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "run-123")
	bootWarmPod(t, ctx, c, kp, gvisorKey, "warm-1", 5, true) // warm, no Secret

	settle := fakeSettle{settled: map[string]bool{"run-123": true}}
	follow := fakeFollow{live: map[string]bool{}}

	report, err := warmpool.SweepSettledRunOwned(ctx, c, settle, follow, gvisorKey)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Reaped != 1 {
		t.Errorf("reaped = %d, want 1 (the settled run-owned pod)", report.Reaped)
	}
	if report.Adopted != 0 {
		t.Errorf("adopted = %d, want 0 (backstop never adopts)", report.Adopted)
	}
	assertPodGone(t, ctx, c, "default", "run-owned")
	assertPodExists(t, ctx, c, "default", "warm-1") // warm pod untouched
}
