// changeref_unit_test.go — the NO-Postgres unit lane for the M1.5 change-report
// write (ISI-4131): the fail-closed input guards that return BEFORE any
// BeginTx, plus the kind enum. Same split as workitemwrite_unit_test.go — the
// database-backed properties (append-only round-trip, audit co-commit, FK) are
// exercised by the taskio integration lane against the SHIPPED 0015 migration.
package coord

import (
	"context"
	"errors"
	"testing"
)

func TestAppendChangeRefRejectsBadInput(t *testing.T) {
	db := offlineDB()
	cases := map[string]struct{ workItemID, author, runID, kind, ref string }{
		"no item":    {"", "agent-A", "run-1", "commit", "abc123"},
		"no author":  {"wi-1", "", "run-1", "commit", "abc123"},
		"no ref":     {"wi-1", "agent-A", "run-1", "commit", ""},
		"bad kind":   {"wi-1", "agent-A", "run-1", "wiki_edit", "abc123"},
		"empty kind": {"wi-1", "agent-A", "run-1", "", "abc123"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := AppendChangeRef(context.Background(), db, tc.workItemID, tc.author, tc.runID, tc.kind, tc.ref, "note")
			if !errors.Is(err, ErrInvalidChangeRef) {
				t.Fatalf("got %v, want ErrInvalidChangeRef", err)
			}
		})
	}
}

func TestAppendChangeRefNilDB(t *testing.T) {
	if _, err := AppendChangeRef(context.Background(), nil, "wi-1", "agent-A", "run-1", "commit", "abc", ""); err == nil {
		t.Fatal("nil db must be rejected")
	}
}

// The kind enum is closed: commit + pull_request only.
func TestValidChangeKinds(t *testing.T) {
	for _, ok := range []string{ChangeKindCommit, ChangeKindPullRequest} {
		if !ValidChangeKinds[ok] {
			t.Fatalf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Commit", "PULL_REQUEST", "patch", "artifact"} {
		if ValidChangeKinds[bad] {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}

func TestNewWorkItemReadStoreNilDB(t *testing.T) {
	if _, err := NewWorkItemReadStore(nil); err == nil {
		t.Fatal("nil db must be rejected")
	}
}

// The pre-DB input guards of the read store fail closed with a plain error
// (never a silent empty result).
func TestWorkItemReadStoreRequiresIDs(t *testing.T) {
	s, err := NewWorkItemReadStore(offlineDB())
	if err != nil {
		t.Fatalf("NewWorkItemReadStore: %v", err)
	}
	if _, err := s.ListWorkItems(context.Background(), "", ""); err == nil {
		t.Fatal("ListWorkItems without projectID must fail")
	}
	if _, err := s.ReadWorkItemThread(context.Background(), "", ""); err == nil {
		t.Fatal("ReadWorkItemThread without workItemID must fail")
	}
}
