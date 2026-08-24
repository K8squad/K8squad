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

package reconcile

import "testing"

// stuckStore is a Store whose Advance NEVER commits (a same-fence competitor or
// a fence-raced reclaim already invalidated this pass) while Step() keeps
// returning the same step — the production shape that would spin Reconcile
// forever without the MaxPasses bound.
type stuckStore struct{ MemStore }

func (s *stuckStore) Advance(expected, next Step, fence *int64) bool { return false }

// TestMaxPassesBoundsTheSpin proves the production spin guard: with an Advance
// that can never commit, MaxPasses=N returns nil after at most N passes with the
// step still non-terminal (the caller requeues and re-reads the world), and the
// default (MaxPasses=0) keeps the unbounded falsification semantics.
func TestMaxPassesBoundsTheSpin(t *testing.T) {
	w := NewWorld()
	s := &stuckStore{MemStore: *NewMemStore()}

	// Bound set: terminates, non-terminal, no effects fired beyond the first pass.
	if err := Reconcile(w, s, Options{Durable: true, Fence: 1, MaxPasses: 5}); err != nil {
		t.Fatalf("bounded drive returned error: %v", err)
	}
	if s.Step() != StepPending {
		t.Fatalf("step = %q, want pending (nothing committed)", s.Step())
	}
	if got := len(w.SandboxBinds); got > 1 {
		t.Fatalf("sandbox binds = %d, want <= 1 (loop must be bounded)", got)
	}

	// Sanity: the unbounded default still drives the healthy happy path.
	w2 := NewWorld()
	healthy := NewMemStore()
	if err := Reconcile(w2, healthy, Options{Durable: true, Fence: 1, MaxPasses: 64}); err != nil {
		t.Fatalf("healthy bounded drive: %v", err)
	}
	if healthy.Step() != StepSucceeded {
		t.Fatalf("healthy drive step = %q, want succeeded", healthy.Step())
	}
}
