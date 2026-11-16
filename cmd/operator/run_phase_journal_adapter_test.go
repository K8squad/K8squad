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
	"sync"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	runctrl "github.com/K8squad/K8squad/pkg/controller/run"
)

// edge is a tiny constructor for a PhaseEdge carrying valid tokens + tenancy so
// coord.RecordRunPhaseTransition's validation passes through to the mock INSERT.
func edge(runUID, project, from, to string) runctrl.PhaseEdge {
	return runctrl.PhaseEdge{RunUID: runUID, ProjectID: project, FromPhase: from, ToPhase: to}
}

// TestRunPhaseJournalAdapter_SuppressesConsecutiveSameEdge mirrors the 16:28Z
// incident: run 97a0ddc9 re-presented claiming→running on every reconcile pass
// (449 rows in 76s). The suppressor must collapse the storm to ONE insert, then
// record the next DIFFERENT edge, and (because that edge is terminal) evict the
// run so a later legitimate claiming→running for the same UID records again.
func TestRunPhaseJournalAdapter_SuppressesConsecutiveSameEdge(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	run := uuid.NewString()
	project := uuid.NewString()

	// Exactly three inserts are expected across the whole scenario:
	//   1. first claiming→running (the storm collapses to this one)
	//   2. running→complete (a real, different, terminal edge → evicts the run)
	//   3. claiming→running again (post-eviction, a fresh run life → records anew)
	for i := 0; i < 3; i++ {
		mock.ExpectExec("INSERT INTO coord.run_phase_transition").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	a := newRunPhaseJournalAdapter(db)
	ctx := context.Background()

	// 50 identical claiming→running passes — the storm.
	for i := 0; i < 50; i++ {
		if err := a.RecordRunPhaseTransition(ctx, edge(run, project, "claiming", "running")); err != nil {
			t.Fatalf("storm pass %d: %v", i, err)
		}
	}
	// A real terminal edge — records, then evicts the run's dedup slot.
	if err := a.RecordRunPhaseTransition(ctx, edge(run, project, "running", "complete")); err != nil {
		t.Fatalf("terminal edge: %v", err)
	}
	if _, ok := a.lastEdge[run]; ok {
		t.Fatalf("terminal edge did not evict run %s from dedup map", run)
	}
	// Post-eviction, the same edge records again (new run life, not a storm dup).
	if err := a.RecordRunPhaseTransition(ctx, edge(run, project, "claiming", "running")); err != nil {
		t.Fatalf("post-eviction edge: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected exactly 3 inserts: %v", err)
	}
}

// TestRunPhaseJournalAdapter_PerRunIsolation proves the dedup key is per Run UID:
// an identical edge for a DIFFERENT run is never suppressed.
func TestRunPhaseJournalAdapter_PerRunIsolation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	runA, runB := uuid.NewString(), uuid.NewString()
	project := uuid.NewString()

	// Two inserts: one per distinct run. The second claiming→running for runA is
	// the suppressed dup; runB's identical edge is a separate run and must land.
	mock.ExpectExec("INSERT INTO coord.run_phase_transition").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO coord.run_phase_transition").WillReturnResult(sqlmock.NewResult(0, 1))

	a := newRunPhaseJournalAdapter(db)
	ctx := context.Background()

	if err := a.RecordRunPhaseTransition(ctx, edge(runA, project, "claiming", "running")); err != nil {
		t.Fatalf("runA first: %v", err)
	}
	if err := a.RecordRunPhaseTransition(ctx, edge(runA, project, "claiming", "running")); err != nil {
		t.Fatalf("runA dup: %v", err)
	}
	if err := a.RecordRunPhaseTransition(ctx, edge(runB, project, "claiming", "running")); err != nil {
		t.Fatalf("runB: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected exactly 2 inserts (one per run): %v", err)
	}
}

// TestRunPhaseJournalAdapter_WriteFailureDoesNotPoisonDedup proves a failed insert
// leaves no dedup memory, so the controller's retry of the same edge is recorded
// rather than silently dropped (the row never landed, so it is not a duplicate).
func TestRunPhaseJournalAdapter_WriteFailureDoesNotPoisonDedup(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	run := uuid.NewString()
	project := uuid.NewString()

	mock.ExpectExec("INSERT INTO coord.run_phase_transition").WillReturnError(errors.New("boom"))
	mock.ExpectExec("INSERT INTO coord.run_phase_transition").WillReturnResult(sqlmock.NewResult(0, 1))

	a := newRunPhaseJournalAdapter(db)
	ctx := context.Background()

	if err := a.RecordRunPhaseTransition(ctx, edge(run, project, "claiming", "running")); err == nil {
		t.Fatal("expected the first (failing) insert to return an error")
	}
	if err := a.RecordRunPhaseTransition(ctx, edge(run, project, "claiming", "running")); err != nil {
		t.Fatalf("retry after failure should record, got: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected 2 inserts (fail then retry): %v", err)
	}
}

// TestRunPhaseJournalAdapter_ConcurrentSameEdge exercises the dedup map under
// concurrent passes (run with `go test -race`). The adapter deliberately does NOT
// hold its mutex across the DB write — journal writes across runs must not
// serialize on one lock, and controller-runtime already serializes reconciles per
// Run object, so concurrent first-sightings of one run's edge cannot happen in
// production. The contract this test pins: no data race on the map, and a
// consistent final dedup state (the non-terminal edge is remembered exactly once).
func TestRunPhaseJournalAdapter_ConcurrentSameEdge(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	run := uuid.NewString()
	project := uuid.NewString()

	// Allow any number of concurrent inserts to land without an "unexpected call"
	// error; we assert map consistency, not an exact row count (see doc above).
	for i := 0; i < 32; i++ {
		mock.ExpectExec("INSERT INTO coord.run_phase_transition").WillReturnResult(sqlmock.NewResult(0, 1))
	}

	a := newRunPhaseJournalAdapter(db)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.RecordRunPhaseTransition(ctx, edge(run, project, "claiming", "running"))
		}()
	}
	wg.Wait()

	if got := a.lastEdge[run]; got != "claiming\x00running" {
		t.Fatalf("dedup map inconsistent after concurrent passes: %q", got)
	}
}
