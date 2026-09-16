// workitemdispatch_unit_test.go — the NO-Postgres unit lane for the board
// dispatch store (ADR-0022 / ISI-4411): the constructor guards and the
// fail-closed input guards that return BEFORE any BeginTx, plus — since
// ISI-4573 — the sqlmock lane pinning the locked-lane branch table end-to-end
// (backlog advance unchanged, todo+unclaimed re-assign, todo+claimed 409,
// post-todo lanes 409, membership/tenancy guards, CAS conflict). The
// database-backed properties against a live Postgres are exercised by the 0021
// migration self-check and the apiserver handler tests; here every `go test
// ./...` re-proves the branch logic without a server.
package coord

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// stubAgents is a TeamAgentResolver that never has to be consulted for the
// pre-BeginTx guards (they return first). It records whether it WAS consulted so
// a guard that leaks past the input check trips the test.
type stubAgents struct {
	names  []string
	err    error
	called bool
}

func (s *stubAgents) TeamAgents(_ context.Context, _ string) ([]string, error) {
	s.called = true
	return s.names, s.err
}

func newOfflineDispatchStore(t *testing.T, agents TeamAgentResolver) *WorkItemDispatchStore {
	t.Helper()
	s, err := NewWorkItemDispatchStore(offlineDB(), agents)
	if err != nil {
		t.Fatalf("NewWorkItemDispatchStore: %v", err)
	}
	return s
}

func TestNewWorkItemDispatchStoreRejectsNilDeps(t *testing.T) {
	if _, err := NewWorkItemDispatchStore(nil, &stubAgents{}); err == nil {
		t.Fatal("nil db must be rejected")
	}
	if _, err := NewWorkItemDispatchStore(offlineDB(), nil); err == nil {
		t.Fatal("nil team-agent resolver must be rejected (it is the §D4 auth source)")
	}
}

// TestRequestDispatchRejectsBadInput — the required-field guards fail closed with
// ErrInvalidWorkItem and never reach the (offline) DB or consult the resolver.
func TestRequestDispatchRejectsBadInput(t *testing.T) {
	cases := map[string]RequestDispatchInput{
		"no work item": {AgentID: "coder", Principal: "user:alice"},
		"no agent":     {WorkItemID: "wi-1", Principal: "user:alice"},
		"no principal": {WorkItemID: "wi-1", AgentID: "coder"},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			agents := &stubAgents{}
			s := newOfflineDispatchStore(t, agents)
			_, err := s.RequestDispatch(context.Background(), in)
			if !errors.Is(err, ErrInvalidWorkItem) {
				t.Fatalf("want ErrInvalidWorkItem, got %v", err)
			}
			if agents.called {
				t.Fatal("resolver must not be consulted before the input guards pass")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ISI-4573 sqlmock lane — the locked-lane branch table of RequestDispatch,
// exercised through the real store code over a scripted driver: backlog advance
// (pinned byte-identical), todo+unclaimed re-assign, todo+claimed 409, post-todo
// lanes 409, and the guards/CAS the re-assign branch must keep identical.
// ---------------------------------------------------------------------------

const (
	dispTeam = "44444444-4444-4444-4444-444444444444"
	dispItem = "11111111-1111-1111-1111-111111111111"
	dispRun  = "99999999-9999-9999-9999-999999999999"
)

// SQL fragments matched with QuoteMeta so $ placeholders are literal.
var (
	dispLockSQL  = regexp.QuoteMeta(`SELECT state, team_id FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`)
	dispClaimSQL = regexp.QuoteMeta(`SELECT run_id FROM coord.claim WHERE work_item_id = $1::uuid FOR UPDATE`)
	// The backlog CAS moves the lane; the re-assign CAS does NOT (state stays todo).
	dispBacklogUpdateSQL = regexp.QuoteMeta(`SET requested_agent = $2, state = 'todo', updated_at = now()`)
	dispReassignUpdateQL = regexp.QuoteMeta(`SET requested_agent = $2, updated_at = now()`)
	dispAuditSQL         = regexp.QuoteMeta(`INSERT INTO coord.audit_log`)
)

// newDispatchSUT wires the real store over a sqlmock db plus a stub resolver.
func newDispatchSUT(t *testing.T, agents *stubAgents) (*WorkItemDispatchStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewWorkItemDispatchStore(db, agents)
	if err != nil {
		t.Fatalf("NewWorkItemDispatchStore: %v", err)
	}
	return s, mock
}

// dispLockRows scripts the step-1 lock read: state + owning team.
func dispLockRows(state, team string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"state", "team_id"}).AddRow(state, team)
}

// dispAuditPayload is the exact §6.5 JSON the store must co-commit (same shape
// for dispatch and re-assign; json.Marshal orders map keys alphabetically).
func dispAuditPayload(agent string) string {
	b, _ := json.Marshal(map[string]any{"initiator": "human", "requested_agent": agent})
	return string(b)
}

// dispBase is a valid dispatch/re-assign input for the item under test.
func dispBase() RequestDispatchInput {
	return RequestDispatchInput{WorkItemID: dispItem, AgentID: "coder", Principal: "user:alice"}
}

// TestRequestDispatchBacklogAdvancePinned — the backlog path keeps its exact
// write shape after ISI-4573: CAS advance `state='backlog'→'todo'` + a
// 'work_item_dispatch_requested' audit row backlog→todo. Regression pin that the
// re-assign branch changed nothing here.
func TestRequestDispatchBacklogAdvancePinned(t *testing.T) {
	agents := &stubAgents{names: []string{"coder"}}
	s, mock := newDispatchSUT(t, agents)

	mock.ExpectBegin()
	mock.ExpectQuery(dispLockSQL).WithArgs(dispItem).
		WillReturnRows(dispLockRows("backlog", dispTeam))
	mock.ExpectExec(dispBacklogUpdateSQL).WithArgs(dispItem, "coder").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(dispAuditSQL).
		WithArgs(dispItem, "work_item_dispatch_requested", "user:alice", nil, "backlog", "todo", dispAuditPayload("coder")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, err := s.RequestDispatch(context.Background(), dispBase())
	if err != nil {
		t.Fatalf("backlog dispatch: %v", err)
	}
	if got.FromState != "backlog" || got.ToState != "todo" || got.RequestedAgent != "coder" {
		t.Fatalf("result: %+v", got)
	}
	if !agents.called {
		t.Fatal("membership resolver must be consulted on the backlog path")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRequestDispatchTodoUnclaimedReassignOK — the ISI-4573 window: a todo item
// whose coord.claim carries no run_id gets requested_agent swapped IN PLACE —
// no lane move (CAS re-asserts state='todo', the UPDATE never sets state), a
// 'work_item_reassign_requested' audit row with from==to=="todo", result
// {todo,todo}, same membership guard.
func TestRequestDispatchTodoUnclaimedReassignOK(t *testing.T) {
	agents := &stubAgents{names: []string{"reviewer"}}
	s, mock := newDispatchSUT(t, agents)
	in := dispBase()
	in.AgentID = "reviewer"

	mock.ExpectBegin()
	mock.ExpectQuery(dispLockSQL).WithArgs(dispItem).
		WillReturnRows(dispLockRows("todo", dispTeam))
	mock.ExpectQuery(dispClaimSQL).WithArgs(dispItem).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(nil))
	mock.ExpectExec(dispReassignUpdateQL).WithArgs(dispItem, "reviewer").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(dispAuditSQL).
		WithArgs(dispItem, "work_item_reassign_requested", "user:alice", nil, "todo", "todo", dispAuditPayload("reviewer")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, err := s.RequestDispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("todo re-assign: %v", err)
	}
	if got.FromState != "todo" || got.ToState != "todo" {
		t.Fatalf("re-assign must not move the lane: %+v", got)
	}
	if got.RequestedAgent != "reviewer" {
		t.Fatalf("requested agent not swapped: %+v", got)
	}
	if !agents.called {
		t.Fatal("membership resolver must be consulted on the re-assign path too")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRequestDispatchTodoClaimedConflict — a todo item whose claim already
// names a run is a clean 409 naming the state; NO intent write, NO audit.
func TestRequestDispatchTodoClaimedConflict(t *testing.T) {
	agents := &stubAgents{names: []string{"coder"}}
	s, mock := newDispatchSUT(t, agents)

	mock.ExpectBegin()
	mock.ExpectQuery(dispLockSQL).WithArgs(dispItem).
		WillReturnRows(dispLockRows("todo", dispTeam))
	mock.ExpectQuery(dispClaimSQL).WithArgs(dispItem).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(dispRun))
	mock.ExpectRollback()

	_, err := s.RequestDispatch(context.Background(), dispBase())
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("todo+claimed: want ErrStateConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), `"todo"`) {
		t.Fatalf("409 must name the state, got %v", err)
	}
	if agents.called {
		t.Fatal("membership is moot once the claimed-run precondition fails")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRequestDispatchPostTodoLanesConflict — in_progress / in_review / done are
// past the door: 409 naming the state, before the claim read or the resolver.
func TestRequestDispatchPostTodoLanesConflict(t *testing.T) {
	for _, state := range []string{"in_progress", "in_review", "done"} {
		t.Run(state, func(t *testing.T) {
			agents := &stubAgents{names: []string{"coder"}}
			s, mock := newDispatchSUT(t, agents)

			mock.ExpectBegin()
			mock.ExpectQuery(dispLockSQL).WithArgs(dispItem).
				WillReturnRows(dispLockRows(state, dispTeam))
			mock.ExpectRollback()

			_, err := s.RequestDispatch(context.Background(), dispBase())
			if !errors.Is(err, ErrStateConflict) {
				t.Fatalf("%s: want ErrStateConflict, got %v", state, err)
			}
			if !strings.Contains(err.Error(), `"`+state+`"`) {
				t.Fatalf("409 must name the state %q, got %v", state, err)
			}
			if agents.called {
				t.Fatal("resolver must not be consulted once the lane precondition fails")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRequestDispatchReassignAgentNotInTeam — the §3 D4 membership guard covers
// the re-assign branch identically: a non-member target agent is 403 and the
// item (requested_agent included) is left untouched.
func TestRequestDispatchReassignAgentNotInTeam(t *testing.T) {
	agents := &stubAgents{names: []string{"someone-else"}}
	s, mock := newDispatchSUT(t, agents)

	mock.ExpectBegin()
	mock.ExpectQuery(dispLockSQL).WithArgs(dispItem).
		WillReturnRows(dispLockRows("todo", dispTeam))
	mock.ExpectQuery(dispClaimSQL).WithArgs(dispItem).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(nil))
	// NO ExpectExec: a non-member never gets an intent write.
	mock.ExpectRollback()

	_, err := s.RequestDispatch(context.Background(), dispBase())
	if !errors.Is(err, ErrAgentNotInTeam) {
		t.Fatalf("want ErrAgentNotInTeam, got %v", err)
	}
	if !agents.called {
		t.Fatal("resolver must have been consulted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRequestDispatchReassignTenancyNotFound — §12.1 existence-hiding holds on
// the todo branch too: a team-scoped caller re-assigning a foreign item gets
// 404 BEFORE the claim read (no cross-tenant signal leakage).
func TestRequestDispatchReassignTenancyNotFound(t *testing.T) {
	agents := &stubAgents{names: []string{"coder"}}
	s, mock := newDispatchSUT(t, agents)
	in := dispBase()
	in.TeamID = "55555555-5555-5555-5555-555555555555" // a foreign team scope

	mock.ExpectBegin()
	mock.ExpectQuery(dispLockSQL).WithArgs(dispItem).
		WillReturnRows(dispLockRows("todo", dispTeam)) // item belongs to another team
	// NO claim-read expectation: tenancy short-circuits first.
	mock.ExpectRollback()

	_, err := s.RequestDispatch(context.Background(), in)
	if !errors.Is(err, ErrWorkItemNotFound) {
		t.Fatalf("want ErrWorkItemNotFound, got %v", err)
	}
	if agents.called {
		t.Fatal("resolver must not be consulted for a 404 tenancy miss")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRequestDispatchReassignCASConflict — the re-assign UPDATE re-asserts
// state='todo'; 0 rows (a racing lane change slipped past the lock read) is the
// shared ErrStateConflict, never a silent clobber.
func TestRequestDispatchReassignCASConflict(t *testing.T) {
	agents := &stubAgents{names: []string{"coder"}}
	s, mock := newDispatchSUT(t, agents)

	mock.ExpectBegin()
	mock.ExpectQuery(dispLockSQL).WithArgs(dispItem).
		WillReturnRows(dispLockRows("todo", dispTeam))
	mock.ExpectQuery(dispClaimSQL).WithArgs(dispItem).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(nil))
	mock.ExpectExec(dispReassignUpdateQL).WithArgs(dispItem, "coder").
		WillReturnResult(sqlmock.NewResult(0, 0)) // CAS matched nothing
	mock.ExpectRollback()

	_, err := s.RequestDispatch(context.Background(), dispBase())
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("CAS miss: want ErrStateConflict, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
