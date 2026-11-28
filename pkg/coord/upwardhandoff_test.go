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
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/pkg/coord"
)

const (
	upItem    = "11111111-1111-1111-1111-111111111111"
	upRun     = "22222222-2222-2222-2222-222222222222"
	upProject = "33333333-3333-3333-3333-333333333333"
	upTeam    = "55555555-5555-5555-5555-555555555555"
	upPrincip = "ksquad-operator"
)

// The FIRST writer commits the full spine in the caller's transaction: the
// at-most-once marker, the 'upward_handoff_pushed' audit row, and the
// manager's wake work item (state 'todo', requested_agent = the manager). The
// complete expectation set IS the atomicity proof: every statement rides the
// one transaction, and none may run after another fails.
func TestPushUpwardHandoff_FirstWriterPushesMarkerAuditWake(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT assignee_agent FROM coord.claim").
		WithArgs(upItem).
		WillReturnRows(sqlmock.NewRows([]string{"assignee_agent"}).AddRow("ic-sam"))
	mock.ExpectQuery("SELECT project_id::text, team_id::text, title").
		WithArgs(upItem).
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "team_id", "title"}).
			AddRow(upProject, upTeam, "Ship the thing"))
	mock.ExpectQuery("SELECT kind, ref").
		WithArgs(upItem, upRun).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "ref", "summary"}).
			AddRow("pull_request", "https://github.com/K8squad/K8squad/pull/1095", "merged"))
	mock.ExpectQuery("SELECT kind, uri").
		WithArgs(upItem, upRun).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "uri", "sha256"}))
	mock.ExpectQuery("INSERT INTO coord.upward_handoff").
		WithArgs(upItem, upRun, "ic-sam", "mgr-boss", "pr", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"work_item_id"}).AddRow(upItem))
	mock.ExpectExec("INSERT INTO coord.audit_log").
		WithArgs(upItem, upRun, upPrincip, "pr", "ic-sam", "mgr-boss").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO coord.work_item").
		WithArgs(upProject, upTeam, upItem,
			"Hand-off (pr) from ic-sam: Ship the thing", sqlmock.AnyArg(), upPrincip, "mgr-boss").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := coord.PushUpwardHandoff(context.Background(), tx, upItem, upRun, upPrincip, "succeeded", "mgr-boss"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A re-entry (the marker already exists) is a silent no-op: the reads happen,
// the ON CONFLICT INSERT returns no row, and NO audit row and NO wake item
// follow — at-most-once, nothing double-fires.
func TestPushUpwardHandoff_ReentryIsSilentNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT assignee_agent FROM coord.claim").
		WithArgs(upItem).
		WillReturnRows(sqlmock.NewRows([]string{"assignee_agent"}).AddRow("ic-sam"))
	mock.ExpectQuery("SELECT project_id::text, team_id::text, title").
		WithArgs(upItem).
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "team_id", "title"}).
			AddRow(upProject, upTeam, "Ship the thing"))
	mock.ExpectQuery("SELECT kind, ref").
		WithArgs(upItem, upRun).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "ref", "summary"}).
			AddRow("commit", "abc123", ""))
	mock.ExpectQuery("SELECT kind, uri").
		WithArgs(upItem, upRun).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "uri", "sha256"}))
	mock.ExpectQuery("INSERT INTO coord.upward_handoff").
		WithArgs(upItem, upRun, "ic-sam", "mgr-boss", "ticket", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"work_item_id"})) // conflict: no row back
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := coord.PushUpwardHandoff(context.Background(), tx, upItem, upRun, upPrincip, "succeeded", "mgr-boss"); err != nil {
		t.Fatalf("re-entry push: %v (want silent no-op)", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The unarmed gates are free calls: no manager, a failed/cancelled terminal,
// or a runless settle write NOTHING (nil tx never even errors — the gates run
// first). A nil tx with the spine armed is the one hard error.
func TestPushUpwardHandoff_NoOpGates(t *testing.T) {
	ctx := context.Background()
	if err := coord.PushUpwardHandoff(ctx, nil, upItem, upRun, upPrincip, "succeeded", ""); err != nil {
		t.Fatalf("no manager: %v (want nil)", err)
	}
	if err := coord.PushUpwardHandoff(ctx, nil, upItem, upRun, upPrincip, "failed", "mgr-boss"); err != nil {
		t.Fatalf("failed terminal: %v (want nil)", err)
	}
	if err := coord.PushUpwardHandoff(ctx, nil, upItem, upRun, upPrincip, "cancelled", "mgr-boss"); err != nil {
		t.Fatalf("cancelled terminal: %v (want nil)", err)
	}
	if err := coord.PushUpwardHandoff(ctx, nil, upItem, "", upPrincip, "succeeded", "mgr-boss"); err != nil {
		t.Fatalf("runless settle: %v (want nil)", err)
	}
	if err := coord.PushUpwardHandoff(ctx, nil, upItem, upRun, upPrincip, "succeeded", "mgr-boss"); err == nil {
		t.Fatal("nil tx with the spine armed: want error")
	}
}

// No assignee on the checkout: nothing to hand off FROM — the claim read
// returns NULL and the push is a silent no-op (no marker, no audit, no wake).
func TestPushUpwardHandoff_NoAssigneeNoPush(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT assignee_agent FROM coord.claim").
		WithArgs(upItem).
		WillReturnRows(sqlmock.NewRows([]string{"assignee_agent"}).AddRow(nil))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := coord.PushUpwardHandoff(context.Background(), tx, upItem, upRun, upPrincip, "succeeded", "mgr-boss"); err != nil {
		t.Fatalf("no assignee: %v (want nil)", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A marker INSERT failure fails the push loudly (the caller fails its whole
// terminal transaction): no audit row, no wake item may follow.
func TestPushUpwardHandoff_MarkFailureFailsLoud(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT assignee_agent FROM coord.claim").
		WithArgs(upItem).
		WillReturnRows(sqlmock.NewRows([]string{"assignee_agent"}).AddRow("ic-sam"))
	mock.ExpectQuery("SELECT project_id::text, team_id::text, title").
		WithArgs(upItem).
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "team_id", "title"}).
			AddRow(upProject, upTeam, "Ship the thing"))
	mock.ExpectQuery("SELECT kind, ref").
		WithArgs(upItem, upRun).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "ref", "summary"}))
	mock.ExpectQuery("SELECT kind, uri").
		WithArgs(upItem, upRun).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "uri", "sha256"}))
	mock.ExpectQuery("INSERT INTO coord.upward_handoff").
		WithArgs(upItem, upRun, "ic-sam", "mgr-boss", "done", sqlmock.AnyArg()).
		WillReturnError(fmt.Errorf("db down"))

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := coord.PushUpwardHandoff(context.Background(), tx, upItem, upRun, upPrincip, "succeeded", "mgr-boss"); err == nil {
		t.Fatal("marker failure: want error")
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
