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

const hbDueQuery = `SELECT work_item_id::text, NULLIF(run_id::text,''), holder_principal, fence_token, reconcile_step, acquired_at
		  FROM coord.claim
		 WHERE holder_principal IS NOT NULL`

// hbCols is the due-query column set (ISI-5438 added acquired_at).
var hbCols = []string{"work_item_id", "run_id", "holder_principal", "fence_token", "reconcile_step", "acquired_at"}

// An in-flight held claim gets its lease renewed with the row's exact
// (item, principal, run, fence) tuple.
func TestHeartbeatSweepRenewsInFlight(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(hbCols).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(7), "running", nil)
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

	rows := sqlmock.NewRows(hbCols).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(3), "succeeded", nil)
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

	rows := sqlmock.NewRows(hbCols).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(3), "failed", nil)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE coord.claim`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.outbox`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE coord.work_item`).
		WithArgs("11111111-1111-1111-1111-111111111111", "todo").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	s := &HeartbeatSweeper{DB: db, Claimer: &fakeRenewer{}}
	s.sweep(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A killed run reclaimed via the heartbeat backstop lands on the CANCELLED
// terminal lane (ISI-4489/Q3), not todo — the same coord.SettleLaneOf mapping
// the settle uses, guarded on in_progress so a human move is never clobbered.
func TestHeartbeatSweepReleasesTerminalCancelledLandsCancelled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(hbCols).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(3), "cancelled", nil)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE coord.claim`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.outbox`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE coord.work_item`).
		WithArgs("11111111-1111-1111-1111-111111111111", "cancelled").
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

	rows := sqlmock.NewRows(hbCols).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(1), "dispatching", nil)
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

// ---- ISI-5184: orphan-claim verification (Run CR gone → release, not renew) ----

// fakeRuns fakes the runExistenceChecker seam.
type fakeRuns struct {
	live map[string]bool
	err  error
}

func (f *fakeRuns) RunExists(_ context.Context, runID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.live[runID], nil
}

// A held claim whose backing Run CR still exists is renewed as before.
func TestHeartbeatSweepVerifiesThenRenewsLiveRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(hbCols).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(7), "running", nil)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	rn := &fakeRenewer{ret: true}
	s := &HeartbeatSweeper{DB: db, Claimer: rn, Runs: &fakeRuns{live: map[string]bool{"22222222-2222-2222-2222-222222222222": true}}}
	s.sweep(context.Background())

	if len(rn.calls) != 1 {
		t.Fatalf("live run must be renewed; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A held claim whose backing Run CR no longer exists is NOT renewed: the
// orphan-release transaction clears custody, audits claim_released with the
// orphan verdict, emits the outbox event, and returns the lane to todo.
func TestHeartbeatSweepReleasesOrphanClaim(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(hbCols).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(9), "dispatching", nil)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE coord.claim`).
		WithArgs("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.audit_log`).
		WithArgs("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222",
			OperatorPrincipal, int64(9), "orphan_run_deleted").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.outbox`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE coord.work_item`).
		WithArgs("11111111-1111-1111-1111-111111111111", "todo").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	rn := &fakeRenewer{ret: true}
	s := &HeartbeatSweeper{DB: db, Claimer: rn, Runs: &fakeRuns{live: map[string]bool{}}}
	s.sweep(context.Background())

	if len(rn.calls) != 0 {
		t.Fatalf("an orphan claim must never be renewed; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// Verification errors fail safe: no renewal, no release — the next tick retries.
func TestHeartbeatSweepVerificationErrorSkipsRenewal(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(hbCols).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(7), "running", nil)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	rn := &fakeRenewer{ret: true}
	s := &HeartbeatSweeper{DB: db, Claimer: rn,
		Runs: &fakeRuns{err: fmt.Errorf("apiserver unavailable")}}
	s.sweep(context.Background())

	if len(rn.calls) != 0 {
		t.Fatalf("verification failure must skip renewal; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// Nil verifier keeps the pre-ISI-5184 blind-renew behavior (legacy wiring).
func TestHeartbeatSweepNilRunsBlindRenews(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(hbCols).
		AddRow("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", OperatorPrincipal, int64(7), "running", nil)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	rn := &fakeRenewer{ret: true}
	s := &HeartbeatSweeper{DB: db, Claimer: rn}
	s.sweep(context.Background())

	if len(rn.calls) != 1 {
		t.Fatalf("nil verifier must blind-renew; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// ---- ISI-5438: zombie-run gate (Run CR present but sandbox pod gone → release) ----

const (
	zItem = "11111111-1111-1111-1111-111111111111"
	zRun  = "22222222-2222-2222-2222-222222222222"
)

// fakeLive fakes the runLivenessChecker seam.
type fakeLive struct {
	live map[string]bool
	err  error
}

func (f *fakeLive) HasLiveSandbox(_ context.Context, runID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.live[runID], nil
}

// base + helpers: a fixed clock so the stall grace is deterministic.
var zNow = time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)

func zSweeper(rn *fakeRenewer, live *fakeLive) *HeartbeatSweeper {
	return &HeartbeatSweeper{
		Claimer: rn,
		Runs:    &fakeRuns{live: map[string]bool{zRun: true}}, // CR exists
		Live:    live,
		Now:     func() time.Time { return zNow },
	}
}

// A Run CR that still exists but whose sandbox pod is GONE, held past the stall
// grace, is NOT renewed: the zombie-release transaction clears custody, forces
// step=failed, audits claim_released with the zombie verdict, and returns the
// lane to todo.
func TestHeartbeatSweepReleasesZombieNoSandbox(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	acquired := zNow.Add(-30 * time.Minute) // older than the 10m grace
	rows := sqlmock.NewRows(hbCols).
		AddRow(zItem, zRun, OperatorPrincipal, int64(9), "running", acquired)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE coord.claim`).
		WithArgs(zItem, zRun).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.audit_log`).
		WithArgs(zItem, zRun, OperatorPrincipal, int64(9), "zombie_no_sandbox").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.outbox`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE coord.work_item`).
		WithArgs(zItem, "todo").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	rn := &fakeRenewer{ret: true}
	s := zSweeper(rn, &fakeLive{live: map[string]bool{}}) // no live pod
	s.DB = db
	s.sweep(context.Background())

	if len(rn.calls) != 0 {
		t.Fatalf("a zombie claim must never be renewed; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A Run with a live sandbox pod is renewed as normal (not a zombie).
func TestHeartbeatSweepRenewsRunWithLiveSandbox(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	acquired := zNow.Add(-30 * time.Minute)
	rows := sqlmock.NewRows(hbCols).
		AddRow(zItem, zRun, OperatorPrincipal, int64(7), "running", acquired)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	rn := &fakeRenewer{ret: true}
	s := zSweeper(rn, &fakeLive{live: map[string]bool{zRun: true}})
	s.DB = db
	s.sweep(context.Background())

	if len(rn.calls) != 1 {
		t.Fatalf("a run with a live pod must be renewed; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A pod-less run still inside the stall grace (just claimed, pod booting) is
// RENEWED, never reaped — the boot window is protected.
func TestHeartbeatSweepRenewsPodlessWithinGrace(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	acquired := zNow.Add(-1 * time.Minute) // well inside the 10m grace
	rows := sqlmock.NewRows(hbCols).
		AddRow(zItem, zRun, OperatorPrincipal, int64(7), "claiming", acquired)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	rn := &fakeRenewer{ret: true}
	s := zSweeper(rn, &fakeLive{live: map[string]bool{}}) // no pod yet
	s.DB = db
	s.sweep(context.Background())

	if len(rn.calls) != 1 {
		t.Fatalf("a booting run inside the grace must be renewed; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A PAUSED run with no pod is never a zombie: park() keeps/forgoes its pod by
// design and the wait is intentional. It is renewed, not reaped.
func TestHeartbeatSweepNeverReapsPausedRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	acquired := zNow.Add(-2 * time.Hour) // long past the grace
	rows := sqlmock.NewRows(hbCols).
		AddRow(zItem, zRun, OperatorPrincipal, int64(7), "paused(rate_limited)", acquired)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	rn := &fakeRenewer{ret: true}
	s := zSweeper(rn, &fakeLive{live: map[string]bool{}})
	s.DB = db
	s.sweep(context.Background())

	if len(rn.calls) != 1 {
		t.Fatalf("a paused run must be renewed, never zombie-reaped; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A sandbox-liveness error fails SAFE: renew, never release on a maybe.
func TestHeartbeatSweepLivenessErrorRenews(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	acquired := zNow.Add(-30 * time.Minute)
	rows := sqlmock.NewRows(hbCols).
		AddRow(zItem, zRun, OperatorPrincipal, int64(7), "running", acquired)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	rn := &fakeRenewer{ret: true}
	s := zSweeper(rn, &fakeLive{err: fmt.Errorf("apiserver unavailable")})
	s.DB = db
	s.sweep(context.Background())

	if len(rn.calls) != 1 {
		t.Fatalf("a liveness error must fall through to renew; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A row with no acquired_at anchor is never flagged (stall check fails closed):
// the pod-less run is renewed, not reaped.
func TestHeartbeatSweepNoAcquiredAtNeverZombie(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(hbCols).
		AddRow(zItem, zRun, OperatorPrincipal, int64(7), "running", nil) // NULL acquired_at
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	rn := &fakeRenewer{ret: true}
	s := zSweeper(rn, &fakeLive{live: map[string]bool{}})
	s.DB = db
	s.sweep(context.Background())

	if len(rn.calls) != 1 {
		t.Fatalf("no acquired_at anchor must fail closed to renew; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// Nil Live keeps the pre-ISI-5438 behavior: a pod-less, long-held in-flight run
// is still blind-renewed (no zombie gate wired).
func TestHeartbeatSweepNilLiveNoZombieGate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	acquired := zNow.Add(-2 * time.Hour)
	rows := sqlmock.NewRows(hbCols).
		AddRow(zItem, zRun, OperatorPrincipal, int64(7), "running", acquired)
	mock.ExpectQuery(regexp.QuoteMeta(hbDueQuery)).WillReturnRows(rows)

	rn := &fakeRenewer{ret: true}
	s := &HeartbeatSweeper{DB: db, Claimer: rn,
		Runs: &fakeRuns{live: map[string]bool{zRun: true}},
		Now:  func() time.Time { return zNow }} // Live nil
	s.sweep(context.Background())

	if len(rn.calls) != 1 {
		t.Fatalf("nil Live must blind-renew; calls = %v", rn.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
