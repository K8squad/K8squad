// initialfindings_unit_test.go — the NO-Postgres unit lane for the ADR-0029
// Option B initial-findings write (ISI-5603): the fail-closed input guards that
// return BEFORE any BeginTx. Same split as changeref_unit_test.go — the
// database-backed properties (comment append + 'initial_findings_authored' audit
// co-commit in one txn, FK on a dangling item) are exercised by the taskio
// integration lane against the shipped coord schema.
package coord

import (
	"context"
	"testing"
)

func TestAppendInitialFindingsRejectsBadInput(t *testing.T) {
	db := offlineDB()
	cases := map[string]struct{ workItemID, author, runID, body string }{
		"no item":   {"", "agent-A", "run-1", "found X"},
		"no author": {"wi-1", "", "run-1", "found X"},
		"no body":   {"wi-1", "agent-A", "run-1", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// All three guards return before BeginTx, so the offline db is never touched.
			if _, err := AppendInitialFindings(context.Background(), db, tc.workItemID, tc.author, tc.runID, tc.body); err == nil {
				t.Fatalf("expected a validation error for %s", name)
			}
		})
	}
}

func TestAppendInitialFindingsNilDB(t *testing.T) {
	if _, err := AppendInitialFindings(context.Background(), nil, "wi-1", "agent-A", "run-1", "found X"); err == nil {
		t.Fatal("nil db must be rejected")
	}
}
