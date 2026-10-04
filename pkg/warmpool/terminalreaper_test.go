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

	"github.com/K8squad/K8squad/pkg/warmpool"
)

// ISI-5438 — SweepTerminalRunOwned garbage-collects the sandbox pods the
// settlement reaper cannot: a Cancelled/Failed run never writes an a2a
// settled_at, so its pod leaks and pins the project RWO PVC (the live 2d5h
// leak). Selection is by the AnnRunID stamp alone; the oracle decides
// terminal-or-gone and fails CLOSED on error.

// fakeOracle is the RunPhaseOracle test double.
type fakeOracle struct {
	done map[string]bool // runID → terminal-or-gone
	err  error
}

func (f fakeOracle) TerminalOrGone(_ context.Context, runID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.done[runID], nil
}

// A run-owned pod whose Run is terminal (or gone) is reaped — the exact leaked
// Cancelled-sandbox case.
func TestTerminalReaperReapsTerminalRun(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "run-123")

	oracle := fakeOracle{done: map[string]bool{"run-123": true}} // terminal/gone

	report, err := warmpool.SweepTerminalRunOwned(ctx, c, oracle)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Reaped != 1 {
		t.Errorf("reaped = %d, want 1 (terminal run ⇒ reap leaked sandbox)", report.Reaped)
	}
	assertPodGone(t, ctx, c, "default", "run-owned")
}

// A run-owned pod whose Run is still live is KEPT — run-drive owns its teardown.
func TestTerminalReaperKeepsLiveRun(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "run-123")

	oracle := fakeOracle{done: map[string]bool{}} // not terminal

	report, err := warmpool.SweepTerminalRunOwned(ctx, c, oracle)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Reaped != 0 || report.LeftRunOwned != 1 {
		t.Errorf("report = %+v, want 0 reaped / 1 leftRunOwned (live run ⇒ keep)", report)
	}
	assertPodExists(t, ctx, c, "default", "run-owned")
}

// A warm/unbound pod (no AnnRunID stamp) is never touched — only run-owned pods
// are this sweep's concern.
func TestTerminalReaperIgnoresWarmPods(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	bootWarmPod(t, ctx, c, kp, gvisorKey, "warm-1", 5, true) // warm, no AnnRunID

	// Oracle would say terminal for everything — but the warm pod has no stamp,
	// so it is never queried and never reaped.
	oracle := fakeOracle{done: map[string]bool{"": true}}

	report, err := warmpool.SweepTerminalRunOwned(ctx, c, oracle)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Reaped != 0 {
		t.Errorf("reaped = %d, want 0 (warm pods are not run-owned)", report.Reaped)
	}
	assertPodExists(t, ctx, c, "default", "warm-1")
}

// An oracle error fails CLOSED: the pod is kept (never reaped on a maybe) and the
// error is surfaced so the operator logs it.
func TestTerminalReaperOracleErrorKeepsAndReports(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "run-123")

	oracle := fakeOracle{err: errors.New("apiserver down")}

	report, err := warmpool.SweepTerminalRunOwned(ctx, c, oracle)
	if err == nil {
		t.Fatal("want a surfaced oracle error, got nil")
	}
	if report.Reaped != 0 {
		t.Errorf("reaped = %d, want 0 (oracle error ⇒ never reap)", report.Reaped)
	}
	assertPodExists(t, ctx, c, "default", "run-owned")
}

// A nil oracle makes the sweep inert (fail-safe default when not wired).
func TestTerminalReaperNilOracleInert(t *testing.T) {
	ctx := context.Background()
	c, kp := newFakeKube(t)
	seedRunOwnedPod(t, ctx, c, kp, "run-owned", "run-123")

	report, err := warmpool.SweepTerminalRunOwned(ctx, c, nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Reaped != 0 {
		t.Errorf("reaped = %d, want 0 (nil oracle ⇒ inert)", report.Reaped)
	}
	assertPodExists(t, ctx, c, "default", "run-owned")
}
