// workitemcomment_test.go — ISI-7837: the retry circuit-breaker reset on kick-off.
//
// The live incident (work item a11d4f7c-…): a circuit-breaker-parked ticket sat on
// ’todo’ + blocked_reason=retry_circuit_breaker_max_generations_10, and the user’s
// kick-off comment posted 201 while NOTHING re-enabled dispatch — DueWorkItems
// filters blocked_reason IS NOT NULL, so no Run was ever minted again (the park had
// no reset path at all). These tests pin the new kick-off reset: the comment
// transaction clears OUR breaker park (never a human block / needs_approval),
// stamps the §6.5 work_item_unparked audit row, and reports BreakerReset on the
// outcome so the console can seed the dispatch ladder. The breaker’s generation
// count itself resets through breaker_floor (migration 0046, intake.go), which the
// rundrive pair TestIntakeSweepBreakerFloorResetsOnUnpark /
// TestIntakeSweepBreakerReArmsAboveFloor pins.
package coord

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const breakerResetReason = "retry_circuit_breaker_max_generations_10"

func newCommentTestStore(t *testing.T) (*WorkItemWriteStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewWorkItemWriteStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemWriteStore: %v", err)
	}
	return store, mock
}

// commentStmt regexes: distinctive fragments of each statement the kick-off path
// issues (sqlmock matches partially, in call order).
var (
	commentSelectQ = regexp.QuoteMeta(`SELECT wi.state, wi.team_id::text, wi.project_id::text, wi.blocked_reason, c.holder_principal`)
	commentInsertQ = regexp.QuoteMeta(`INSERT INTO coord.comment (work_item_id, author_principal, body, payload)`)
	commentAuditQ  = regexp.QuoteMeta(`INSERT INTO coord.audit_log (work_item_id, event_type, principal, initiated_by_user_id, payload)`)
	unparkQ        = regexp.QuoteMeta(`SET blocked_reason = NULL, updated_at = now()`)
	nudgeQ         = regexp.QuoteMeta(`SET state = 'todo', updated_at = now()`)
	transitionQ    = regexp.QuoteMeta(`VALUES ($1::uuid, 'state_transition', $2, $3, 'todo', $4::jsonb)`)
)

// TestAppendHumanCommentBreakerResetOnKickoff is the ISI-7837 repro: a
// breaker-parked ticket on ’todo’ (the park never moves the lane) whose kick-off
// comment must clear OUR park and report BreakerReset — the exact silent no-op
// (comment 201, no run ever minted) the live board hit.
func TestAppendHumanCommentBreakerResetOnKickoff(t *testing.T) {
	const (
		itemID = "11111111-1111-1111-1111-111111111111"
		teamID = "22222222-2222-2222-2222-222222222222"
	)
	store, mock := newCommentTestStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(commentSelectQ).WithArgs(itemID).
		WillReturnRows(sqlmock.NewRows([]string{"state", "team_id", "project_id", "blocked_reason", "holder_principal"}).
			AddRow("todo", teamID, "proj-1", breakerResetReason, nil))
	mock.ExpectQuery(commentInsertQ).WithArgs(itemID, "user:me", "kick it off", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Now()))
	mock.ExpectExec(commentAuditQ).WithArgs(itemID, "work_item_commented", "user:me", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The kick-off reset: the park is cleared, CAS’d on the exact reason read
	// under the FOR UPDATE lock.
	mock.ExpectExec(unparkQ).WithArgs(itemID, breakerResetReason).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(commentAuditQ).WithArgs(itemID, "work_item_unparked", "user:me", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// ’todo’ already IS the dispatch lane: no nudge, no state_transition.
	mock.ExpectCommit()

	outcome, err := store.AppendHumanComment(context.Background(), itemID, teamID, "user:me", "kick it off", nil)
	if err != nil {
		t.Fatalf("AppendHumanComment: %v", err)
	}
	if !outcome.BreakerReset {
		t.Fatal("a kick-off on a breaker-parked ticket must report BreakerReset")
	}
	if outcome.ReTriggered {
		t.Fatal("a ticket parked on todo needs no lane nudge; ReTriggered must stay false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAppendHumanCommentNonBreakerParkStaysBlocked pins the never-clobber half:
// a human block / needs_approval is NOT cleared by a comment — the kick-off
// lands, the park persists (the console renders the blocked badge instead).
func TestAppendHumanCommentNonBreakerParkStaysBlocked(t *testing.T) {
	const (
		itemID = "11111111-1111-1111-1111-111111111111"
		teamID = "22222222-2222-2222-2222-222222222222"
	)
	store, mock := newCommentTestStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(commentSelectQ).WithArgs(itemID).
		WillReturnRows(sqlmock.NewRows([]string{"state", "team_id", "project_id", "blocked_reason", "holder_principal"}).
			AddRow("todo", teamID, "proj-1", "needs_approval", nil))
	mock.ExpectQuery(commentInsertQ).WithArgs(itemID, "user:me", "a nudge", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Now()))
	mock.ExpectExec(commentAuditQ).WithArgs(itemID, "work_item_commented", "user:me", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// No un-park UPDATE, no work_item_unparked audit: the park survives the
	// comment (fail-closed, never a clobber of a human’s block).
	mock.ExpectCommit()

	outcome, err := store.AppendHumanComment(context.Background(), itemID, teamID, "user:me", "a nudge", nil)
	if err != nil {
		t.Fatalf("AppendHumanComment: %v", err)
	}
	if outcome.BreakerReset {
		t.Fatal("a non-breaker park must NOT be cleared by a comment")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAppendHumanCommentBreakerResetAlsoNudgesLane covers the driver-parked
// shape: a breaker park stamped while the ticket sat on a working lane
// (’in_progress’, no live holder) gets BOTH the reset and the ISI-4495 lane
// nudge back to ’todo’ — un-park and dispatch in one kick-off.
func TestAppendHumanCommentBreakerResetAlsoNudgesLane(t *testing.T) {
	const (
		itemID = "11111111-1111-1111-1111-111111111111"
		teamID = "22222222-2222-2222-2222-222222222222"
	)
	store, mock := newCommentTestStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(commentSelectQ).WithArgs(itemID).
		WillReturnRows(sqlmock.NewRows([]string{"state", "team_id", "project_id", "blocked_reason", "holder_principal"}).
			AddRow("in_progress", teamID, "proj-1", breakerResetReason, nil))
	mock.ExpectQuery(commentInsertQ).WithArgs(itemID, "user:me", "kick it off", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Now()))
	mock.ExpectExec(commentAuditQ).WithArgs(itemID, "work_item_commented", "user:me", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(unparkQ).WithArgs(itemID, breakerResetReason).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(commentAuditQ).WithArgs(itemID, "work_item_unparked", "user:me", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(nudgeQ).WithArgs(itemID, "in_progress").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(transitionQ).WithArgs(itemID, "user:me", "in_progress", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	outcome, err := store.AppendHumanComment(context.Background(), itemID, teamID, "user:me", "kick it off", nil)
	if err != nil {
		t.Fatalf("AppendHumanComment: %v", err)
	}
	if !outcome.BreakerReset || !outcome.ReTriggered || outcome.ToState != "todo" {
		t.Fatalf("a driver-parked kick-off must reset AND nudge; got %+v", outcome)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
