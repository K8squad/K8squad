/*
Copyright 2026 The K8sAgentMesh Authors.

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

package mesh

import (
	"context"
	"testing"
	"time"
)

// Test battery re-homed from the reference host's supervisor loop tests
// (rundrive meshsupervisor tests, ISI-6010 P1.4b): the Postgres store is
// swapped for the in-memory fakeRegistry; the loop contract is unchanged.

// newSweeper returns a Sweeper bound to a fake registry.
func newSweeper(t *testing.T, opts ...SweeperOption) (*Sweeper, *fakeRegistry) {
	t.Helper()
	reg := newFakeRegistry()
	s, err := NewSweeper(reg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s, reg
}

// Construction guard: nil registry is rejected.
func TestNewSweeper_NilRegistry(t *testing.T) {
	if _, err := NewSweeper(nil); err == nil {
		t.Fatal("expected nil-registry error")
	}
}

// The sweeper loop ticks on its configured interval and calls Sweep on the
// registry (the TTL-expiry prune path: the registry never grows unbounded
// with dead gateways).
func TestSweeper_SweepOnTick(t *testing.T) {
	s, reg := newSweeper(t, WithSweeperInterval(10*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := reg.sweepCount(); n < 2 {
		t.Fatalf("sweeps = %d, want >= 2 in the window", n)
	}
}

// Sweep failures are tolerated but do not stop the loop — the next tick
// retries.
func TestSweeper_SweepErrorContinues(t *testing.T) {
	s, reg := newSweeper(t, WithSweeperInterval(10*time.Millisecond))
	reg.mu.Lock()
	reg.errSweep = context.DeadlineExceeded
	reg.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := reg.sweepCount(); n < 2 {
		t.Fatalf("sweeps after error = %d, want the loop to continue (>= 2)", n)
	}
}

// The sweeper stops cleanly on context cancellation.
func TestSweeper_Shutdown(t *testing.T) {
	s, reg := newSweeper(t, WithSweeperInterval(time.Hour))
	// With a long interval, the first sweep won't happen before we cancel,
	// so no Sweep is expected. The loop exits cleanly on ctx.Done().

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := reg.sweepCount(); n != 0 {
		t.Fatalf("sweeps = %d, want 0 (cancelled before the first tick)", n)
	}
}

// WithSweeperClock allows tests to pin the clock so TTL computations are
// deterministic without real-time sleeps.
func TestWithSweeperClock(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	s, _ := newSweeper(t,
		WithSweeperClock(func() time.Time { return now }),
		WithSweeperInterval(time.Millisecond),
	)
	if s.now().Year() != 2024 {
		t.Fatal("clock override did not take effect")
	}
}
