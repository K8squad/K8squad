// agentauthor_unit_test.go — the NO-Postgres unit lane for the ADR-0024 agent
// authoring store (ISI-4734): the fail-closed input/root guards that return before
// any BeginTx, and the sqlmock lane pinning the four custody invariants end-to-end
// (I1 sub-tickets only, I2 parent-in-custody, I3 depth cap, I4 per-run budget) plus
// the happy-path create with its honest run-stamped audit row. The database-backed
// properties against a live Postgres are exercised by the migration self-checks and
// the memory-edge handler tests; here every `go test ./...` re-proves the gates.
package coord

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	aaTeam   = "44444444-4444-4444-4444-444444444444"
	aaParent = "22222222-2222-2222-2222-222222222222"
	aaChild  = "33333333-3333-3333-3333-333333333333"
	aaRun    = "99999999-9999-9999-9999-999999999999"
)

// SQL fragments matched with QuoteMeta so $ placeholders stay literal. Each is a
// distinctive prefix so the ordered sqlmock script maps one expectation per query.
var (
	aaParentLockSQL = regexp.QuoteMeta(`SELECT project_id::text, team_id::text, requested_agent`)
	aaClaimSQL      = regexp.QuoteMeta(`SELECT holder_principal, run_id::text FROM coord.claim WHERE work_item_id = $1::uuid`)
	aaDepthSQL      = regexp.QuoteMeta(`SELECT parent_id::text FROM coord.work_item WHERE id = $1::uuid`)
	aaBudgetSQL     = regexp.QuoteMeta(`SELECT count(*) FROM coord.audit_log`)
	aaInsertSQL     = regexp.QuoteMeta(`INSERT INTO coord.work_item`)
	aaAuditSQL      = regexp.QuoteMeta(`INSERT INTO coord.audit_log`)
)

func aaIdentity() AgentIdentity {
	return AgentIdentity{Principal: "agent:pm", AgentID: "pm", RunID: aaRun, TeamID: aaTeam}
}

func aaInput() AgentCreateChildInput {
	return AgentCreateChildInput{ParentID: aaParent, Title: "story: decompose"}
}

// newAuthorSUT wires the real author store over a sqlmock db. The write/dispatch
// stores share the same handle; assign is nil (unavailable) in this unit lane.
func newAuthorSUT(t *testing.T) (*AgentAuthorStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	writes, err := NewWorkItemWriteStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemWriteStore: %v", err)
	}
	s, err := NewAgentAuthorStore(db, writes, nil)
	if err != nil {
		t.Fatalf("NewAgentAuthorStore: %v", err)
	}
	return s, mock
}

// parentLockRows scripts the step-1 parent lock: project, team, requested_agent.
func parentLockRows(requestedAgent any) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"project_id", "team_id", "requested_agent"}).
		AddRow("proj-uuid", aaTeam, requestedAgent)
}

// insertReturnRows scripts the create INSERT ... RETURNING (13 columns).
func insertReturnRows() *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{
		"id", "project_id", "team_id", "parent_id", "title", "body", "state",
		"created_by", "created_at", "updated_at", "priority", "work_mode", "labels",
	}).AddRow(aaChild, "proj-uuid", aaTeam, aaParent, "story: decompose", nil, "backlog",
		"agent:pm", now, now, nil, nil, "{}")
}

func TestNewAgentAuthorStoreRejectsNilCoreDeps(t *testing.T) {
	writes, _ := NewWorkItemWriteStore(offlineDB())
	if _, err := NewAgentAuthorStore(nil, writes, nil); err == nil {
		t.Fatal("nil db must be rejected")
	}
	if _, err := NewAgentAuthorStore(offlineDB(), nil, nil); err == nil {
		t.Fatal("nil write store must be rejected")
	}
	// nil dispatch is allowed (create/update-only deployment).
	if _, err := NewAgentAuthorStore(offlineDB(), writes, nil); err != nil {
		t.Fatalf("nil dispatch must be allowed: %v", err)
	}
}

// TestAgentCreateRootForbidden (I1) — a create with no parent is refused before any
// DB work: root items stay human-only.
func TestAgentCreateRootForbidden(t *testing.T) {
	s, mock := newAuthorSUT(t)
	in := aaInput()
	in.ParentID = ""
	_, err := s.AgentCreateChild(context.Background(), aaIdentity(), in)
	if !errors.Is(err, ErrAgentRootForbidden) {
		t.Fatalf("want ErrAgentRootForbidden, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no DB work expected: %v", err)
	}
}

// TestAgentCreateRejectsBadIdentity — missing run/agent/principal fails closed with
// ErrInvalidWorkItem before any DB work (WINV2: authoring needs a server identity).
func TestAgentCreateRejectsBadIdentity(t *testing.T) {
	cases := map[string]AgentIdentity{
		"no principal": {AgentID: "pm", RunID: aaRun, TeamID: aaTeam},
		"no agent":     {Principal: "agent:pm", RunID: aaRun, TeamID: aaTeam},
		"no run":       {Principal: "agent:pm", AgentID: "pm", TeamID: aaTeam},
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newAuthorSUT(t)
			if _, err := s.AgentCreateChild(context.Background(), id, aaInput()); !errors.Is(err, ErrInvalidWorkItem) {
				t.Fatalf("want ErrInvalidWorkItem, got %v", err)
			}
		})
	}
}

// TestAgentCreateHappyPathViaRequestedAgent (I2 via requested_agent) — the agent is
// the parent's requested_agent, so custody holds WITHOUT a claim read; a root parent
// gives depth 1 (child depth 2 ≤ cap); budget 0; the child is inserted in backlog
// and a run-stamped audit row co-commits.
func TestAgentCreateHappyPathViaRequestedAgent(t *testing.T) {
	s, mock := newAuthorSUT(t)
	mock.ExpectBegin()
	mock.ExpectQuery(aaParentLockSQL).WithArgs(aaParent).WillReturnRows(parentLockRows("pm"))
	// custody via requested_agent ⇒ no claim read.
	mock.ExpectQuery(aaDepthSQL).WithArgs(aaParent).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil)) // parent is a root ⇒ depth 1.
	mock.ExpectQuery(aaBudgetSQL).WithArgs(aaRun).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(aaInsertSQL).WillReturnRows(insertReturnRows())
	mock.ExpectExec(aaAuditSQL).
		WithArgs(aaChild, aaRun, "work_item_created", "agent:pm", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	rec, err := s.AgentCreateChild(context.Background(), aaIdentity(), aaInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.ID != aaChild || rec.State != "backlog" {
		t.Fatalf("record: %+v", rec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAgentCreateNotInCustody (I2) — the agent is not the requested_agent and the
// parent's claim names neither its run nor its principal ⇒ ErrAgentNotInCustody, the
// txn rolls back, nothing is inserted.
func TestAgentCreateNotInCustody(t *testing.T) {
	s, mock := newAuthorSUT(t)
	mock.ExpectBegin()
	mock.ExpectQuery(aaParentLockSQL).WithArgs(aaParent).WillReturnRows(parentLockRows(nil))
	mock.ExpectQuery(aaClaimSQL).WithArgs(aaParent).
		WillReturnRows(sqlmock.NewRows([]string{"holder_principal", "run_id"}).AddRow("someone:else", "00000000-0000-0000-0000-000000000000"))
	mock.ExpectRollback()

	_, err := s.AgentCreateChild(context.Background(), aaIdentity(), aaInput())
	if !errors.Is(err, ErrAgentNotInCustody) {
		t.Fatalf("want ErrAgentNotInCustody, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAgentCreateCrossTenantParent404 — a parent whose team differs from the caller
// is invisible: ErrWorkItemNotFound (existence-hiding), never a cross-tenant 403.
func TestAgentCreateCrossTenantParent404(t *testing.T) {
	s, mock := newAuthorSUT(t)
	mock.ExpectBegin()
	mock.ExpectQuery(aaParentLockSQL).WithArgs(aaParent).
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "team_id", "requested_agent"}).
			AddRow("proj-uuid", "55555555-5555-5555-5555-555555555555", nil))
	mock.ExpectRollback()

	_, err := s.AgentCreateChild(context.Background(), aaIdentity(), aaInput())
	if !errors.Is(err, ErrWorkItemNotFound) {
		t.Fatalf("want ErrWorkItemNotFound, got %v", err)
	}
}

// TestAgentCreateDepthCapExceeded (I3) — the parent already sits at the cap depth, so
// the child would exceed it: ErrAgentDepthCapExceeded, rollback.
func TestAgentCreateDepthCapExceeded(t *testing.T) {
	s, mock := newAuthorSUT(t)
	mock.ExpectBegin()
	mock.ExpectQuery(aaParentLockSQL).WithArgs(aaParent).WillReturnRows(parentLockRows("pm"))
	// Depth walk: parent → p3 → p2 → p1(root). Four nodes ⇒ parent depth 4; child
	// would be 5 > cap 4.
	grand := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	great := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	root := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	mock.ExpectQuery(aaDepthSQL).WithArgs(aaParent).WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(grand))
	mock.ExpectQuery(aaDepthSQL).WithArgs(grand).WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(great))
	mock.ExpectQuery(aaDepthSQL).WithArgs(great).WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(root))
	mock.ExpectQuery(aaDepthSQL).WithArgs(root).WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	mock.ExpectRollback()

	_, err := s.AgentCreateChild(context.Background(), aaIdentity(), aaInput())
	if !errors.Is(err, ErrAgentDepthCapExceeded) {
		t.Fatalf("want ErrAgentDepthCapExceeded, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAgentCreateRunBudgetExceeded (I4) — the run has already authored the budget, so
// the next create is refused: ErrAgentRunBudgetExceeded, rollback.
func TestAgentCreateRunBudgetExceeded(t *testing.T) {
	s, mock := newAuthorSUT(t)
	mock.ExpectBegin()
	mock.ExpectQuery(aaParentLockSQL).WithArgs(aaParent).WillReturnRows(parentLockRows("pm"))
	mock.ExpectQuery(aaDepthSQL).WithArgs(aaParent).WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	mock.ExpectQuery(aaBudgetSQL).WithArgs(aaRun).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(AgentAuthorRunBudget))
	mock.ExpectRollback()

	_, err := s.AgentCreateChild(context.Background(), aaIdentity(), aaInput())
	if !errors.Is(err, ErrAgentRunBudgetExceeded) {
		t.Fatalf("want ErrAgentRunBudgetExceeded, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAgentAssignUnavailableWithoutDispatch — with no dispatch backend the assign
// verb is refused honestly (ErrAgentAssignUnavailable), never a nil-panic, and no DB
// work happens.
func TestAgentAssignUnavailableWithoutDispatch(t *testing.T) {
	s, mock := newAuthorSUT(t)
	_, err := s.AgentAssign(context.Background(), aaIdentity(), aaChild, "coder")
	if !errors.Is(err, ErrAgentAssignUnavailable) {
		t.Fatalf("want ErrAgentAssignUnavailable, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no DB work expected: %v", err)
	}
}

