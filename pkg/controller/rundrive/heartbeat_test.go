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

package rundrive

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// fakeRenewer fakes the §6.2 renew seam.
type fakeRenewer struct {
	calls   []string
	ret     bool
	panicOn bool
}

func (f *fakeRenewer) Renew(_ context.Context, itemID, principal, runID string, fence int64) bool {
	f.calls = append(f.calls, fmt.Sprintf("%s/%s/%s/%d", itemID, principal, runID, fence))
	if f.panicOn {
		panic("infra failure")
	}
	return f.ret
}

const hbDueQuery = `SELECT work_item_id::text, NULLIF(run_id::text,''), holder_principal, fence_token, reconcile_step
		  FROM coord.claim
		 WHERE holder_principal IS NOT NULL`

// An in-flight held claim gets its lease renewed with the row's exact
// (item, principal, run, fence) tuple.
func TestHeartbeatSweepRenewsInFlight(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows([]string{"work_item_id", "run_id", "holder_principal", "fence_token", "reconcile_step"}).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(7), "running")
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	rn := &fakeRenewer{ret: true}
	s := &HeartbeatSweeper{DB: db, Claimer: rn}
	s.sweep(context.Background())

	if len(rn.calls) != 1 || rn.calls[0] != "11111111-1111-1111-1111-111111111111/"+OperatorPrincipal+"/22222222-2222-2222-2222-222222222222/7" {
		t.Fatalf("renew calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A terminal (succeeded) held claim is RELEASED: custody UPDATE +
// claim_released audit + outbox, one transaction — and NO lane write
// (completion lane moves are M1.5's reporting, not custody release).
func TestHeartbeatSweepReleasesTerminalSucceeded(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows([]string{"work_item_id", "run_id", "holder_principal", "fence_token", "reconcile_step"}).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(3), "succeeded")
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE coord.claim`).
		WithArgs("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.audit_log`).
		WithArgs("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222",
			OperatorPrincipal, int64(3), "succeeded").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.outbox`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	rn := &fakeRenewer{}
	s := &HeartbeatSweeper{DB: db, Claimer: rn}
	s.sweep(context.Background())

	if len(rn.calls) != 0 {
		t.Fatalf("a terminal claim must not be renewed; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A FAILED terminal claim also returns the lane to todo (the item is
// reclaimable — the re-enter paths' discipline).
func TestHeartbeatSweepReleasesTerminalFailedReturnsLane(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows([]string{"work_item_id", "run_id", "holder_principal", "fence_token", "reconcile_step"}).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(3), "failed")
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE coord.claim`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.outbox`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE coord.work_item`).
		WithArgs("11111111-1111-1111-1111-111111111111").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	s := &HeartbeatSweeper{DB: db, Claimer: &fakeRenewer{}}
	s.sweep(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// The §6.2 Renew panic contract is CONTAINED: a transient DB failure logs and
// retries next tick — it never kills the sweep (or the operator).
func TestHeartbeatSweepContainsRenewPanic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows([]string{"work_item_id", "run_id", "holder_principal", "fence_token", "reconcile_step"}).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(1), "dispatching")
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	var logs []string
	s := &HeartbeatSweeper{DB: db, Claimer: &fakeRenewer{panicOn: true},
		Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}

	done := make(chan struct{})
	go func() { defer close(done); s.sweep(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a panicking Renew hung or killed the sweep")
	}
	found := false
	for _, l := range logs {
		if containsSub(l, "panicked") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the contained panic must be logged; logs = %v", logs)
	}
}

// A listed-claims failure is logged and swallowed — the next tick retries.
func TestHeartbeatSweepSurvivesDBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnError(fmt.Errorf("db gone"))

	s := &HeartbeatSweeper{DB: db, Claimer: &fakeRenewer{}}
	s.sweep(context.Background()) // must not panic
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// Start honors ctx cancellation (the manager Runnable contract).
func TestHeartbeatStartStopsOnCancel(t *testing.T) {
	s := &HeartbeatSweeper{DB: nil, Claimer: &fakeRenewer{}, Tick: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not honor cancellation")
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
