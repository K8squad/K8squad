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

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/pkg/coord"
)

const (
	stTaskID  = "22222222-2222-2222-2222-222222222222#lap1"
	stRun     = "22222222-2222-2222-2222-222222222222"
	stItem    = "11111111-1111-1111-1111-111111111111"
	stPrincip = "ksquad-operator"
)

// The FIRST writer for a dispatch lap marks it settled and emits exactly one
// 'a2a_settled' audit row — both inside one committed transaction. RETURNING
// work_item_id feeds the audit without a second read.
func TestSettle_FirstWriterMarksAndAudits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE coord.a2a_dispatch").
		WithArgs(stTaskID, coord.SettleOutcomeSucceeded).
		WillReturnRows(sqlmock.NewRows([]string{"work_item_id"}).AddRow(stItem))
	mock.ExpectExec("INSERT INTO coord.audit_log").
		WithArgs(stItem, stRun, stPrincip, coord.SettleOutcomeSucceeded).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	s, err := coord.NewProdSettleWriter(db, stPrincip)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(context.Background(), stTaskID, stRun, coord.SettleOutcomeSucceeded, ""); err != nil {
		t.Fatalf("first Settle: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("first Settle must mark + audit + commit exactly once: %v", err)
	}
}

// At-most-once: a SECOND Settle for the same lap (the conditional UPDATE matches
// nothing — already settled) is a no-op: NO audit row, NO duplicate marker, and
// no error. This is the OnDone-re-entry / restart-boundary case.
func TestSettle_SecondWriterIsNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	// WHERE settled_at IS NULL matches nothing → RETURNING yields no rows.
	mock.ExpectQuery("UPDATE coord.a2a_dispatch").
		WithArgs(stTaskID, coord.SettleOutcomeFailed).
		WillReturnRows(sqlmock.NewRows([]string{"work_item_id"}))
	// NO ExpectExec for the audit INSERT: a no-op settle must not audit.
	mock.ExpectRollback()

	s, err := coord.NewProdSettleWriter(db, stPrincip)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(context.Background(), stTaskID, stRun, coord.SettleOutcomeFailed, ""); err != nil {
		t.Fatalf("re-entrant Settle should be a silent no-op, got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("re-entrant Settle must NOT write a second audit row: %v", err)
	}
}

// An invalid outcome is rejected before any DB work — fail-closed on a value the
// 0019 CHECK would reject anyway, with a clear error and no transaction.
func TestSettle_InvalidOutcomeRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// No expectations: Settle must return before BeginTx.
	s, err := coord.NewProdSettleWriter(db, stPrincip)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(context.Background(), stTaskID, stRun, "bogus", ""); err == nil {
		t.Fatal("Settle must reject an out-of-family outcome")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("an invalid outcome must not touch the DB: %v", err)
	}
}

// A marker-write failure surfaces and aborts before the audit — the audit lands
// only when the marker did.
func TestSettle_MarkErrorSurfacesNoAudit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE coord.a2a_dispatch").
		WithArgs(stTaskID, coord.SettleOutcomeFollowError).
		WillReturnError(errors.New("deadlock detected"))
	mock.ExpectRollback()

	s, err := coord.NewProdSettleWriter(db, stPrincip)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(context.Background(), stTaskID, stRun, coord.SettleOutcomeFollowError, ""); err == nil {
		t.Fatal("a marker-write failure must surface as an error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no audit row may be attempted after a marker failure: %v", err)
	}
}

// ISI-5032 (ISI-5028 hypothesis 3): a FAILED follow with a non-empty reason posts
// one agent-attributed coord.comment (`Run failed: <reason>`) in the SAME
// transaction as the marker + audit, so the ticket thread answers why the run
// died. The agent attribution is read from the checkout row's assignee_agent.
func TestSettle_FailedPostsFailureComment(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE coord.a2a_dispatch").
		WithArgs(stTaskID, coord.SettleOutcomeFailed).
		WillReturnRows(sqlmock.NewRows([]string{"work_item_id"}).AddRow(stItem))
	mock.ExpectExec("INSERT INTO coord.audit_log").
		WithArgs(stItem, stRun, stPrincip, coord.SettleOutcomeFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT assignee_agent FROM coord.claim").
		WithArgs(stItem).
		WillReturnRows(sqlmock.NewRows([]string{"assignee_agent"}).AddRow("sam"))
	mock.ExpectExec("INSERT INTO coord.comment").
		WithArgs(stItem, stPrincip, "Run failed: sandbox readiness refused (a2a task "+stTaskID+") — agent sam").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	s, err := coord.NewProdSettleWriter(db, stPrincip)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(context.Background(), stTaskID, stRun, coord.SettleOutcomeFailed, "sandbox readiness refused"); err != nil {
		t.Fatalf("failed Settle with reason: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a failed settle with a reason must post one failure comment: %v", err)
	}
}

// A FAILED follow with an EMPTY reason posts nothing (nothing new over the
// generic settle summary), and a succeeded/follow_error outcome never posts a
// failure comment even when a reason string is present.
func TestSettle_NoFailureCommentWhenNoReasonOrNotFailed(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
		reason  string
	}{
		{"failed-empty-reason", coord.SettleOutcomeFailed, ""},
		{"succeeded-with-reason", coord.SettleOutcomeSucceeded, "boom"},
		{"follow-error-with-reason", coord.SettleOutcomeFollowError, "stream reset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			mock.ExpectBegin()
			mock.ExpectQuery("UPDATE coord.a2a_dispatch").
				WithArgs(stTaskID, tc.outcome).
				WillReturnRows(sqlmock.NewRows([]string{"work_item_id"}).AddRow(stItem))
			mock.ExpectExec("INSERT INTO coord.audit_log").
				WithArgs(stItem, stRun, stPrincip, tc.outcome).
				WillReturnResult(sqlmock.NewResult(0, 1))
			// No assignee read, no comment INSERT — just commit.
			mock.ExpectCommit()

			s, err := coord.NewProdSettleWriter(db, stPrincip)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Settle(context.Background(), stTaskID, stRun, tc.outcome, tc.reason); err != nil {
				t.Fatalf("Settle: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("no failure comment may be posted for %s: %v", tc.name, err)
			}
		})
	}
}

func TestNewProdSettleWriter_Validation(t *testing.T) {
	if _, err := coord.NewProdSettleWriter(nil, stPrincip); err == nil {
		t.Fatal("nil db must error")
	}
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := coord.NewProdSettleWriter(db, ""); err == nil {
		t.Fatal("empty principal must error")
	}
}

// The S2 reader (ADR-0020 §2.3): Settled is a single indexed EXISTS over
// idx_a2a_dispatch_settled — true when any lap of the run carries the marker,
// false when none does, and a query failure surfaces (the reaper fails closed on
// an error). Keyed by run_id::uuid.
func TestSettleReader_Settled(t *testing.T) {
	cases := []struct {
		name string
		rows bool
		want bool
	}{
		{"settled", true, true},
		{"not-settled", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			mock.ExpectQuery("SELECT EXISTS").
				WithArgs(stRun).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(tc.rows))

			r, err := coord.NewProdSettleReader(db)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.Settled(context.Background(), stRun)
			if err != nil {
				t.Fatalf("Settled: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Settled = %v, want %v", got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unexpected DB interaction: %v", err)
			}
		})
	}
}

func TestSettleReader_QueryErrorSurfaces(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(stRun).
		WillReturnError(errors.New("connection reset"))

	r, err := coord.NewProdSettleReader(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Settled(context.Background(), stRun); err == nil {
		t.Fatal("a query failure must surface (reaper fails closed on error)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB interaction: %v", err)
	}
}

func TestNewProdSettleReader_Validation(t *testing.T) {
	if _, err := coord.NewProdSettleReader(nil); err == nil {
		t.Fatal("nil db must error")
	}
}

// The S3 read path (ADR-0020 §2.4, ISI-4403): SettledForWorkItem returns the
// (dispatched, settled) pair from one aggregate scan of coord.a2a_dispatch keyed
// by work_item_id::uuid. count>0 is dispatched; bool_or(settled_at IS NOT NULL)
// is settled across §8 retry laps.
func TestSettleReader_SettledForWorkItem(t *testing.T) {
	cases := []struct {
		name         string
		dispatched   bool
		settled      bool
		wantDispatch bool
		wantSettled  bool
	}{
		{"dispatched-and-settled", true, true, true, true},
		{"dispatched-in-flight", true, false, true, false},
		{"never-dispatched", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			mock.ExpectQuery("FROM coord.a2a_dispatch").
				WithArgs(stItem).
				WillReturnRows(sqlmock.NewRows([]string{"dispatched", "settled"}).
					AddRow(tc.dispatched, tc.settled))

			r, err := coord.NewProdSettleReader(db)
			if err != nil {
				t.Fatal(err)
			}
			gotD, gotS, err := r.SettledForWorkItem(context.Background(), stItem)
			if err != nil {
				t.Fatalf("SettledForWorkItem: %v", err)
			}
			if gotD != tc.wantDispatch || gotS != tc.wantSettled {
				t.Fatalf("SettledForWorkItem = (%v,%v), want (%v,%v)",
					gotD, gotS, tc.wantDispatch, tc.wantSettled)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unexpected DB interaction: %v", err)
			}
		})
	}
}

// A "" or non-uuid workItemRef (ISI-4354: a malformed ref on a pre-validation CR)
// short-circuits to (false,false) WITHOUT touching the DB — the ::uuid cast would
// otherwise reject it (22P02) and stall the projector in error backoff.
func TestSettleReader_SettledForWorkItem_BadKeyNoDBTouch(t *testing.T) {
	for _, id := range []string{"", "not-a-uuid"} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		r, err := coord.NewProdSettleReader(db)
		if err != nil {
			t.Fatal(err)
		}
		d, s, err := r.SettledForWorkItem(context.Background(), id)
		if err != nil || d || s {
			t.Fatalf("id %q: got (%v,%v,%v), want (false,false,nil) with no DB touch", id, d, s, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("id %q: DB was touched: %v", id, err)
		}
		db.Close()
	}
}

func TestSettleReader_SettledForWorkItem_QueryErrorSurfaces(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.a2a_dispatch").
		WithArgs(stItem).
		WillReturnError(errors.New("connection reset"))

	r, err := coord.NewProdSettleReader(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.SettledForWorkItem(context.Background(), stItem); err == nil {
		t.Fatal("a query failure must surface (projector requeues rather than reading terminal)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB interaction: %v", err)
	}
}
