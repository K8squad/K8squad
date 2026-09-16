// taskdetail_unit_test.go — the NO-Postgres unit lane for the shared richer
// read (ISI-4574): pins that the human's dispatch intent (requested_agent,
// ADR-0022 / mig 0021) is surfaced on the detail read — set when a dispatch
// happened, "" when never dispatched — and that the thread payload the console
// BFF proxies verbatim exposes it under the wire's PascalCase `RequestedAgent`
// name without disturbing the existing fields. The real-Postgres properties
// (actual column nullability, claim join) ride the migration self-check and the
// integration lanes; here sqlmock pins the SELECT/scan contract.
package coord_test

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/pkg/coord"
)

const tdItem = "11111111-1111-1111-1111-111111111111"

// tdDetailCols is the work_item+claim join projection in SELECT order — the
// positional contract ReadTaskDetail scans. requested_agent sits between the
// create-time attributes and the claim columns.
var tdDetailCols = []string{
	"id", "title", "body", "state", "blocked_reason",
	"priority", "work_mode", "labels", "requested_agent",
	"holder_principal", "run_id", "fence_token", "assignee_agent",
}

// expectTaskDetailReads primes the three reads ReadTaskDetail makes: the
// one-row join (the QuoteMeta'd fragment trips if wi.requested_agent is
// dropped from — or reordered within — the SELECT), then the empty comment /
// change-ref threads.
func expectTaskDetailReads(t *testing.T, mock sqlmock.Sqlmock, requestedAgent driver.Value) {
	t.Helper()
	// The dispatched-but-NOT-yet-claimed posture: intent stamped (todo), claim
	// still unheld — exactly the console gap RequestedAgent fills.
	mock.ExpectQuery(regexp.QuoteMeta("wi.priority, wi.work_mode, wi.labels, wi.requested_agent")).
		WithArgs(tdItem).
		WillReturnRows(sqlmock.NewRows(tdDetailCols).AddRow(
			tdItem, "ship the widget", "do the thing", "todo", nil,
			"high", "standard", nil, requestedAgent,
			nil, nil, int64(0), nil,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.comment")).
		WithArgs(tdItem).
		WillReturnRows(sqlmock.NewRows([]string{"author_principal", "body", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.change_ref")).
		WithArgs(tdItem).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "ref", "summary", "author_principal", "run_id", "created_at"}))
}

// TestReadTaskDetailSurfacesRequestedAgent — the dispatch intent is readable on
// the detail BEFORE a claim exists, and reads honestly "" when the item was
// never dispatched. The neighbor assertions pin that the new column did not
// shift the positional scan.
func TestReadTaskDetailSurfacesRequestedAgent(t *testing.T) {
	cases := map[string]struct {
		cell driver.Value
		want string
	}{
		"dispatched (intent stamped)": {"coder", "coder"},
		"never dispatched (NULL)":     {nil, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectTaskDetailReads(t, mock, tc.cell)

			td, err := coord.ReadTaskDetail(context.Background(), db, tdItem)
			if err != nil {
				t.Fatalf("ReadTaskDetail: %v", err)
			}
			if td.RequestedAgent != tc.want {
				t.Fatalf("RequestedAgent = %q, want %q", td.RequestedAgent, tc.want)
			}
			// The scan order around the new column is unchanged.
			if td.WorkItemID != tdItem || td.Title != "ship the widget" || td.State != "todo" {
				t.Fatalf("identity fields = %+v", td)
			}
			if td.Priority != "high" || td.WorkMode != "standard" {
				t.Fatalf("attribute fields = %q/%q, want high/standard", td.Priority, td.WorkMode)
			}
			// Pre-claim posture: no holder yet — RequestedAgent is the only
			// "who" the detail can answer with.
			if td.Holder != "" || td.RunID != "" || td.Assignee != "" || td.FenceToken != 0 {
				t.Fatalf("claim fields = holder %q run %q assignee %q fence %d, want empty/0", td.Holder, td.RunID, td.Assignee, td.FenceToken)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expected the join + comment + change-ref reads: %v", err)
			}
		})
	}
}

// TestWorkItemThreadJSONExposesRequestedAgent — the console detail payload
// (GET /api/work-items/{id}; the BFF proxies verbatim) carries the intent under
// the PascalCase `RequestedAgent` key (from TaskDetail.JSON), and the top-level
// StatusHistory (explicit JSON field) survives the additive change.
func TestWorkItemThreadJSONExposesRequestedAgent(t *testing.T) {
	thread := coord.WorkItemThread{
		TaskDetail: coord.TaskDetail{
			WorkItemID: tdItem, Title: "ship it", State: "todo",
			Priority: "high", RequestedAgent: "coder",
		},
		StatusHistory: []coord.StatusChange{{FromState: "backlog", ToState: "todo", Principal: "user:alice"}},
	}
	raw, err := json.Marshal(thread)
	if err != nil {
		t.Fatalf("marshal thread: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode thread: %v", err)
	}
	if got["RequestedAgent"] != "coder" {
		t.Fatalf("payload RequestedAgent = %v, want coder", got["RequestedAgent"])
	}
	if _, ok := got["statusHistory"]; !ok {
		t.Fatalf("top-level statusHistory field must survive (payload %s)", raw)
	}
	if v, ok := got["StatusHistory"]; ok {
		t.Fatalf("camelCase StatusHistory must not appear as PascalCase (got %v)", v)
	}

	// Never dispatched: the key still renders as "" — stable shape for the console.
	thread.TaskDetail.RequestedAgent = ""
	raw, err = json.Marshal(thread)
	if err != nil {
		t.Fatalf("marshal empty thread: %v", err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode empty thread: %v", err)
	}
	if v, ok := got["RequestedAgent"]; !ok || v != "" {
		t.Fatalf("undispatched payload RequestedAgent = %v (ok %v), want \"\" present", v, ok)
	}
}
