// settle_livecomment_test.go — the NO-Postgres unit lane for the ISI-7274
// (parent ISI-7262 WS-2) settle comment echo: proves the settle's thread rows
// co-commit a run-entity `ticket_comment` outbox event in the SAME
// transaction, so the ticket page's ended-keyed re-fetch cannot miss the
// terminal comment (the machine advances — publishing `ended` — before
// Terminal settles; the echo arrives on the same per-run stream a frame
// later and re-arms the re-fetch). The DB-backed lanes (real schema, real
// transactionality) ride the chaos harness; here sqlmock pins the SQL shape
// and the echo's wire payload contract.
package coord_test

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/pkg/coord"
)

const (
	slItem = "11111111-1111-1111-1111-111111111111"
	slRun  = "22222222-2222-2222-2222-222222222222"
)

var (
	slTime  = time.Unix(1700000000, 0).UTC()
	slAgent = "ksquad-operator"
)

// echoArg matches a `ticket_comment` outbox payload (arg $4 of
// CaptureRunForWorkItem's INSERT ... SELECT) against the durable comment it
// echoes: author, kind, workItemId/runId and the server-stamped comment time
// all ride the frame so the console can route it without a refetch.
type echoArg struct {
	t        *testing.T
	wantKind string
	bodyPart string
}

func (a echoArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	var echo struct {
		WorkItemID string    `json:"workItemId"`
		RunID      string    `json:"runId"`
		Author     string    `json:"author"`
		Body       string    `json:"body"`
		Kind       string    `json:"kind"`
		At         time.Time `json:"at"`
	}
	if err := json.Unmarshal([]byte(s), &echo); err != nil {
		return false
	}
	return echo.WorkItemID == slItem && echo.RunID == slRun && echo.Author == slAgent &&
		echo.Kind == a.wantKind && strings.Contains(echo.Body, a.bodyPart) &&
		echo.At.Unix() == slTime.Unix()
}

func echoPayload(t *testing.T, wantKind, bodyPart string) driver.Value {
	return echoArg{t: t, wantKind: wantKind, bodyPart: bodyPart}
}

// TestLiveCommentEchoWireContract pins the SSE payload shape — the durable
// triple (author/body/at) plus workItemId/runId/kind — so the Go producers and
// the console parser stay field-compatible (same contract as WS-1's
// discussion_message seam).
func TestLiveCommentEchoWireContract(t *testing.T) {
	if coord.LiveCommentEventType != "ticket_comment" {
		t.Fatalf("LiveCommentEventType = %q, want ticket_comment", coord.LiveCommentEventType)
	}
	raw, err := json.Marshal(coord.LiveCommentEcho{
		WorkItemID: slItem, RunID: slRun, Author: slAgent, Body: "b", Kind: "summary", At: slTime,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"workItemId", "runId", "author", "body", "kind", "at"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("payload missing %q: %s", key, raw)
		}
	}
}

// TestSettleEmitsCommentEchoSynthesized drives the machine-path settle for a
// SUCCEEDED run whose agent never summarised: the platform-synthesized summary
// comment and its `ticket_comment` echo commit in the SAME transaction.
func TestSettleEmitsCommentEchoSynthesized(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	mock.MatchExpectationsInOrder(true)

	mock.ExpectBegin()
	// Attribution read (checkout may have cleared the holder; assignee retained).
	mock.ExpectQuery(regexp.QuoteMeta("SELECT assignee_agent FROM coord.claim")).
		WithArgs(slItem).
		WillReturnRows(sqlmock.NewRows([]string{"assignee_agent"}).AddRow("sam"))
	// Summary presence: the agent never posted one.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (")).
		WithArgs(slItem, slRun).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// synthesizeSummary best-effort reads: no change refs, no artifacts.
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.change_ref")).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "ref", "summary"}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM coord.artifact")).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "uri"}))
	// The durable summary row (RETURNING created_at) + the co-committed echo.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.comment")).
		WithArgs(slItem, slAgent, sqlmock.AnyArg(), "summary", slRun).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(slTime))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.outbox")).
		WithArgs(slItem, coord.LiveCommentEventType, slRun,
			echoPayload(t, "summary", "Run summary (auto-generated"), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectCommit()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	lane, moved, err := coord.SettleTerminalLane(ctx, tx, slItem, slRun, slAgent, "succeeded")
	if err != nil {
		t.Fatalf("SettleTerminalLane: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if lane != "" || moved {
		t.Fatalf("succeeded owns no engine lane: lane=%q moved=%v", lane, moved)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestSettleEmitsCommentEchoProgress covers the hasSummary branch for a FAILED
// run: the engine terminal line lands as a PROGRESS row and still echoes.
func TestSettleEmitsCommentEchoProgress(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT assignee_agent FROM coord.claim")).
		WithArgs(slItem).
		WillReturnRows(sqlmock.NewRows([]string{"assignee_agent"}).AddRow("sam"))
	// The failed lane re-dispatch guard reads the item's source.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT source FROM coord.work_item")).
		WithArgs(slItem).
		WillReturnRows(sqlmock.NewRows([]string{"source"}).AddRow(nil))
	// The lane move itself (failed → todo) + its state_transition audit.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE coord.work_item")).
		WithArgs("todo", slItem).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO coord.audit_log")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The agent already summarised: the engine line is a PROGRESS row.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (")).
		WithArgs(slItem, slRun).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.comment")).
		WithArgs(slItem, slAgent, sqlmock.AnyArg(), "progress", slRun).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(slTime))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.outbox")).
		WithArgs(slItem, coord.LiveCommentEventType, slRun,
			echoPayload(t, "progress", "Run failed"), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectCommit()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	lane, moved, err := coord.SettleTerminalLane(ctx, tx, slItem, slRun, slAgent, "failed")
	if err != nil {
		t.Fatalf("SettleTerminalLane: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if lane != "todo" || !moved {
		t.Fatalf("failed settles to todo: lane=%q moved=%v", lane, moved)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestSettleWithoutRunSkipsEcho: a settle with no run correlation leaves the
// comment un-echoed — the projector keys the per-run fan on run_id and
// nothing else correlates the frame.
func TestSettleWithoutRunSkipsEcho(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT assignee_agent FROM coord.claim")).
		WithArgs(slItem).
		WillReturnRows(sqlmock.NewRows([]string{"assignee_agent"}).AddRow("sam"))
	// No summary-presence read (it keys on run_id) and NO outbox insert — the
	// absence of an ExpectQuery for coord.outbox is the assertion.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.comment")).
		WithArgs(slItem, slAgent, sqlmock.AnyArg(), "summary", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(slTime))
	mock.ExpectCommit()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	lane, moved, err := coord.SettleTerminalLane(ctx, tx, slItem, "", slAgent, "succeeded")
	if err != nil {
		t.Fatalf("SettleTerminalLane: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if lane != "" || moved {
		t.Fatalf("succeeded owns no engine lane: lane=%q moved=%v", lane, moved)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
