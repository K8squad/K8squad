// workitemwrite_unit_test.go — the NO-Postgres unit lane for the S3 work-item
// create/edit store (ISI-3959): the fail-closed input guards that return BEFORE
// any BeginTx, plus the pure projection helpers. Same split as credpause_test.go
// (offlineConnector) — the database-backed properties (audit co-commit, tenancy
// 404, expectedUpdatedAt CAS→409, reparent cycle walk) exercise a live Postgres
// via the apiserver handler tests and the coord chaos lane; here we pin only the
// branches that never touch the DB, so every `go test ./...` re-proves them.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// offlineDB yields a *sql.DB whose connection is never established — reusing the
// offlineConnector from credpause_test.go. Every guard under test returns before
// BeginTx, so the handle is only ever checked for non-nil.
func offlineDB() *sql.DB { return sql.OpenDB(offlineConnector{}) }

func newOfflineWriteStore(t *testing.T) *WorkItemWriteStore {
	t.Helper()
	s, err := NewWorkItemWriteStore(offlineDB())
	if err != nil {
		t.Fatalf("NewWorkItemWriteStore: %v", err)
	}
	return s
}

func TestNewWorkItemWriteStoreNilDB(t *testing.T) {
	if _, err := NewWorkItemWriteStore(nil); err == nil {
		t.Fatal("nil db must be rejected")
	}
}

// TestCreateWorkItemRejectsBadInput — the three required-field guards fail closed
// with ErrInvalidWorkItem and never reach the (offline) DB.
func TestCreateWorkItemRejectsBadInput(t *testing.T) {
	s := newOfflineWriteStore(t)
	cases := map[string]CreateWorkItemInput{
		"no project":   {Title: "t", Principal: "user:a"},
		"no title":     {ProjectID: "p", Principal: "user:a"},
		"no principal": {ProjectID: "p", Title: "t"},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.CreateWorkItem(context.Background(), in)
			if !errors.Is(err, ErrInvalidWorkItem) {
				t.Fatalf("got %v, want ErrInvalidWorkItem", err)
			}
		})
	}
}

// TestUpdateWorkItemRejectsBadInput — the pre-BeginTx edit guards (missing id /
// principal, no editable field, blank title) all fail closed.
func TestUpdateWorkItemRejectsBadInput(t *testing.T) {
	s := newOfflineWriteStore(t)
	blank := ""
	title := "ok"
	cases := map[string]struct {
		id string
		in UpdateWorkItemInput
	}{
		"no id":        {"", UpdateWorkItemInput{Title: &title, Principal: "user:a"}},
		"no principal": {"wi-1", UpdateWorkItemInput{Title: &title}},
		"no field":     {"wi-1", UpdateWorkItemInput{Principal: "user:a"}},
		"blank title":  {"wi-1", UpdateWorkItemInput{Title: &blank, Principal: "user:a"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.UpdateWorkItem(context.Background(), tc.id, tc.in)
			if !errors.Is(err, ErrInvalidWorkItem) {
				t.Fatalf("got %v, want ErrInvalidWorkItem", err)
			}
		})
	}
}

// TestNullToPtr — NULL / empty ⇒ nil pointer; a real value ⇒ its pointer.
func TestNullToPtr(t *testing.T) {
	if nullToPtr(sql.NullString{}) != nil {
		t.Fatal("invalid NullString must map to nil")
	}
	if nullToPtr(sql.NullString{String: "", Valid: true}) != nil {
		t.Fatal("valid-but-empty must map to nil (a root/unset column is not \"\")")
	}
	p := nullToPtr(sql.NullString{String: "x", Valid: true})
	if p == nil || *p != "x" {
		t.Fatalf("valid value must round-trip, got %v", p)
	}
}

// TestEditedFields — the audit detail names exactly the fields the caller changed.
func TestEditedFields(t *testing.T) {
	title, body := "t", "b"
	got := editedFields(UpdateWorkItemInput{Title: &title, Body: &body})
	fields, ok := got["fields"].([]string)
	if !ok {
		t.Fatalf("fields not []string: %T", got["fields"])
	}
	if len(fields) != 2 || fields[0] != "title" || fields[1] != "body" {
		t.Fatalf("got %v, want [title body]", fields)
	}
	if len(editedFields(UpdateWorkItemInput{})["fields"].([]string)) != 0 {
		t.Fatal("no changed fields ⇒ empty list")
	}
}
