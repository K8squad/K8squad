// failurecause_test.go — the NO-Postgres unit lane for the ISI-7498 WS-A
// terminal-failure cause reader (ProdSettleReader.FailureCauseForWorkItem).
// sqlmock pins the two-query shape and the cause-selection contract: the a2a
// settle outcome wins, the gone-sandbox reap reason is the fallback, and neither
// present yields "" (the projector then uses its generic message). The DB-backed
// end-to-end path rides the chaos harness.
package coord_test

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/pkg/coord"
)

const fcItem = "33333333-3333-3333-3333-333333333333"

func newFailureCauseReader(t *testing.T) (*coord.ProdSettleReader, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r, err := coord.NewProdSettleReader(db)
	if err != nil {
		t.Fatalf("NewProdSettleReader: %v", err)
	}
	return r, mock
}

// A settled FAILED a2a lap is the cause — the sandbox fallback is not even read.
func TestFailureCauseSettledOutcomeWins(t *testing.T) {
	r, mock := newFailureCauseReader(t)
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.a2a_dispatch")).
		WithArgs(fcItem).
		WillReturnRows(sqlmock.NewRows([]string{"settle_outcome"}).AddRow("failed"))

	got, err := r.FailureCauseForWorkItem(context.Background(), fcItem)
	if err != nil {
		t.Fatalf("FailureCauseForWorkItem: %v", err)
	}
	if got != "the agent run settled failed" {
		t.Fatalf("cause = %q, want the named settle outcome", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations (the audit fallback must NOT run): %v", err)
	}
}

// follow_error is infra, but still a legible terminal cause.
func TestFailureCauseFollowError(t *testing.T) {
	r, mock := newFailureCauseReader(t)
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.a2a_dispatch")).
		WithArgs(fcItem).
		WillReturnRows(sqlmock.NewRows([]string{"settle_outcome"}).AddRow("follow_error"))

	got, err := r.FailureCauseForWorkItem(context.Background(), fcItem)
	if err != nil {
		t.Fatalf("FailureCauseForWorkItem: %v", err)
	}
	if got != "the agent run settled follow_error" {
		t.Fatalf("cause = %q, want the follow_error outcome", got)
	}
}

// No settled lap → fall back to the gone-sandbox reap reason.
func TestFailureCauseFallsBackToSandboxReason(t *testing.T) {
	r, mock := newFailureCauseReader(t)
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.a2a_dispatch")).
		WithArgs(fcItem).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.audit_log")).
		WithArgs(fcItem).
		WillReturnRows(sqlmock.NewRows([]string{"reason"}).AddRow("sandbox pod not found"))

	got, err := r.FailureCauseForWorkItem(context.Background(), fcItem)
	if err != nil {
		t.Fatalf("FailureCauseForWorkItem: %v", err)
	}
	if got != "sandbox pod not found" {
		t.Fatalf("cause = %q, want the gone-sandbox reason", got)
	}
}

// A succeeded latest lap is NOT a failure cause (a later unsettled lap failed);
// skip it and fall back to the sandbox reason, else "".
func TestFailureCauseSkipsSucceededLap(t *testing.T) {
	r, mock := newFailureCauseReader(t)
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.a2a_dispatch")).
		WithArgs(fcItem).
		WillReturnRows(sqlmock.NewRows([]string{"settle_outcome"}).AddRow("succeeded"))
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.audit_log")).
		WithArgs(fcItem).
		WillReturnError(sql.ErrNoRows)

	got, err := r.FailureCauseForWorkItem(context.Background(), fcItem)
	if err != nil {
		t.Fatalf("FailureCauseForWorkItem: %v", err)
	}
	if got != "" {
		t.Fatalf("cause = %q, want empty (succeeded lap is not a cause; no reap reason)", got)
	}
}

// Neither source recorded → "" (generic message fallback), no DB error.
func TestFailureCauseNoneRecorded(t *testing.T) {
	r, mock := newFailureCauseReader(t)
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.a2a_dispatch")).
		WithArgs(fcItem).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.audit_log")).
		WithArgs(fcItem).
		WillReturnRows(sqlmock.NewRows([]string{"reason"}).AddRow(""))

	got, err := r.FailureCauseForWorkItem(context.Background(), fcItem)
	if err != nil {
		t.Fatalf("FailureCauseForWorkItem: %v", err)
	}
	if got != "" {
		t.Fatalf("cause = %q, want empty", got)
	}
}

// A non-uuid work item is "" WITHOUT touching the DB (matches SettledForWorkItem).
// ISI-7527 defect 2: the latest-lap ORDER BY must reference only columns that
// exist on coord.a2a_dispatch (a2a_task_id, work_item_id, run_id, dispatched_at,
// settled_at, settle_outcome — 0005; no id column, and no migration adds one).
// The pre-fix query ordered by a bare id, so every reconcile of a Failed run
// errored live with SQLSTATE 42703. Pin the exact clause so a regression fails
// here instead of in the operator log.
func TestFailureCauseQueryOrdersBySchemaColumns(t *testing.T) {
	r, mock := newFailureCauseReader(t)
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY settled_at DESC, a2a_task_id DESC")).
		WithArgs(fcItem).
		WillReturnRows(sqlmock.NewRows([]string{"settle_outcome"}).AddRow("failed"))

	got, err := r.FailureCauseForWorkItem(context.Background(), fcItem)
	if err != nil {
		t.Fatalf("FailureCauseForWorkItem: %v (the query must ORDER BY existing columns only)", err)
	}
	if got != "the agent run settled failed" {
		t.Fatalf("cause = %q, want the named settle outcome", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestFailureCauseNonUUIDSkipsDB(t *testing.T) {
	r, _ := newFailureCauseReader(t)
	got, err := r.FailureCauseForWorkItem(context.Background(), "not-a-uuid")
	if err != nil || got != "" {
		t.Fatalf("non-uuid: got (%q, %v), want (\"\", nil) with no DB query", got, err)
	}
}
