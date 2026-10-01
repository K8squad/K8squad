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

package scm

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestInMemorySyncHistory_AppendNewestFirstAndScope(t *testing.T) {
	ctx := context.Background()
	s := NewInMemorySyncHistoryStore()

	// Two projects interleaved; each RecordSync is a new append-only row.
	for _, r := range []SyncHistoryRow{
		{ProjectNamespace: "team-a", ProjectName: "x", Kind: SyncKindPoll, RecordCount: 1, Principal: "ksquad-operator"},
		{ProjectNamespace: "team-b", ProjectName: "y", Kind: SyncKindWebhook, RecordCount: 2, Principal: "ksquad-operator"},
		{ProjectNamespace: "team-a", ProjectName: "x", Kind: SyncKindWebhook, RecordCount: 3, Principal: "ksquad-operator"},
	} {
		if err := s.RecordSync(ctx, r); err != nil {
			t.Fatalf("RecordSync: %v", err)
		}
	}

	got, err := s.ListSyncHistory(ctx, "team-a", "x", 10)
	if err != nil {
		t.Fatalf("ListSyncHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("project scoping: want 2 rows for team-a/x, got %d", len(got))
	}
	// Newest-first: the webhook pass (recorded last) comes before the poll pass.
	if got[0].Kind != SyncKindWebhook || got[1].Kind != SyncKindPoll {
		t.Fatalf("newest-first order wrong: %q then %q", got[0].Kind, got[1].Kind)
	}
	if got[0].ID <= got[1].ID {
		t.Fatalf("ids not monotonic/descending: %d then %d", got[0].ID, got[1].ID)
	}
}

func TestInMemorySyncHistory_LimitAndDefaults(t *testing.T) {
	ctx := context.Background()
	s := NewInMemorySyncHistoryStore()
	for i := 0; i < 5; i++ {
		if err := s.RecordSync(ctx, SyncHistoryRow{
			ProjectNamespace: "t", ProjectName: "p", Kind: SyncKindPoll, Principal: "ksquad-operator",
		}); err != nil {
			t.Fatalf("RecordSync: %v", err)
		}
	}
	got, err := s.ListSyncHistory(ctx, "t", "p", 2)
	if err != nil {
		t.Fatalf("ListSyncHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit not honoured: want 2, got %d", len(got))
	}
	// Provider/outcome default on write; synced_at is stamped.
	if got[0].Provider != "github" {
		t.Fatalf("provider default: want github, got %q", got[0].Provider)
	}
	if got[0].Outcome != SyncOutcomeSuccess {
		t.Fatalf("outcome default: want success, got %q", got[0].Outcome)
	}
	if got[0].SyncedAt.IsZero() {
		t.Fatalf("synced_at was not stamped")
	}
}

func TestSQLSyncHistory_RecordSync(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	store := &SQLSyncHistoryStore{db: db, now: func() time.Time { return at }}

	mock.ExpectExec(regexp.QuoteMeta(insertSyncHistorySQL)).
		WithArgs("team-a", "proj-x", "github", "https://github.com/acme/widget", SyncKindPoll, "",
			12, SyncOutcomeSuccess, "ksquad-operator", `{"issues":5}`, at).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := store.RecordSync(context.Background(), SyncHistoryRow{
		ProjectNamespace: "team-a",
		ProjectName:      "proj-x",
		Repo:             "https://github.com/acme/widget",
		Kind:             SyncKindPoll,
		RecordCount:      12,
		Principal:        "ksquad-operator",
		Detail:           json.RawMessage(`{"issues":5}`),
	}); err != nil {
		t.Fatalf("RecordSync: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSQLSyncHistory_ListSyncHistory(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSQLSyncHistoryStore(db)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	rows := sqlmock.NewRows([]string{
		"id", "provider", "repo", "kind", "issue_ref",
		"record_count", "outcome", "principal", "detail", "synced_at",
	}).
		AddRow(int64(2), "github", "https://github.com/acme/widget", SyncKindImport, "acme/widget#42",
			1, SyncOutcomeSuccess, "scm-bridge", []byte(`{"comments":3}`), at).
		AddRow(int64(1), "github", "https://github.com/acme/widget", SyncKindPoll, "",
			12, SyncOutcomeSuccess, "ksquad-operator", nil, at)

	mock.ExpectQuery(regexp.QuoteMeta(listSyncHistorySQL)).
		WithArgs("team-a", "proj-x", 20).
		WillReturnRows(rows)

	got, err := store.ListSyncHistory(context.Background(), "team-a", "proj-x", 0) // 0 → default 20
	if err != nil {
		t.Fatalf("ListSyncHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].Kind != SyncKindImport || got[0].IssueRef != "acme/widget#42" {
		t.Fatalf("row 0 mismatch: %+v", got[0])
	}
	if string(got[0].Detail) != `{"comments":3}` {
		t.Fatalf("row 0 detail mismatch: %s", got[0].Detail)
	}
	if got[1].Detail != nil {
		t.Fatalf("row 1 detail should be nil for NULL, got %s", got[1].Detail)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
