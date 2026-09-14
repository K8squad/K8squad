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

package coord_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/pkg/coord"
)

// The reader returns one ReattachTarget per unsettled, non-terminal, in-window
// dispatch lap — the (a2aTaskID, runID) pair Submit needs to reattach.
func TestUnsettledDispatches_ReturnsLaps(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.a2a_dispatch").
		WithArgs((6 * time.Hour).Seconds()).
		WillReturnRows(sqlmock.NewRows([]string{"a2a_task_id", "run_id"}).
			AddRow("run-a#lap2", "run-a").
			AddRow("run-b", "run-b"))

	r, err := coord.NewProdReattachReader(db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.UnsettledDispatches(context.Background(), 6*time.Hour)
	if err != nil {
		t.Fatalf("UnsettledDispatches: %v", err)
	}
	want := []coord.ReattachTarget{
		{A2ATaskID: "run-a#lap2", RunID: "run-a"},
		{A2ATaskID: "run-b", RunID: "run-b"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d targets, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("target[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("query expectations: %v", err)
	}
}

// A non-positive window disables re-attach without touching the DB: no query,
// nil result — the fail-safe for a misconfigured window.
func TestUnsettledDispatches_NonPositiveWindowIsNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// No expectations: the reader must return before QueryContext.
	r, err := coord.NewProdReattachReader(db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.UnsettledDispatches(context.Background(), 0)
	if err != nil {
		t.Fatalf("UnsettledDispatches(0): %v", err)
	}
	if got != nil {
		t.Fatalf("window 0 must return nil, got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("window 0 must issue no query: %v", err)
	}
}

// An empty result set (nothing to re-attach) is a clean nil, no error.
func TestUnsettledDispatches_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.a2a_dispatch").
		WithArgs((time.Hour).Seconds()).
		WillReturnRows(sqlmock.NewRows([]string{"a2a_task_id", "run_id"}))

	r, _ := coord.NewProdReattachReader(db)
	got, err := r.UnsettledDispatches(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("UnsettledDispatches: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty result must yield no targets, got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("query expectations: %v", err)
	}
}

// A query error surfaces wrapped, never a partial list.
func TestUnsettledDispatches_QueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.a2a_dispatch").
		WithArgs((time.Hour).Seconds()).
		WillReturnError(errors.New("boom"))

	r, _ := coord.NewProdReattachReader(db)
	if _, err := r.UnsettledDispatches(context.Background(), time.Hour); err == nil {
		t.Fatal("expected a query error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("query expectations: %v", err)
	}
}

func TestNewProdReattachReader_NilDB(t *testing.T) {
	if _, err := coord.NewProdReattachReader(nil); err == nil {
		t.Fatal("nil db must error")
	}
}
