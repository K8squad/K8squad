// coordstore_commenttc_test.go — the sqlmock unit lane for the ISI-7274
// (parent ISI-7262 WS-2) task-io comment echo: a run's final comments
// (PostComment plain / PostSummary kind='summary') emit the run-entity
// `ticket_comment` outbox event AFTER the durable append commits, best-effort
// — a lost echo costs liveness, never correctness — and an empty runID skips
// the echo. The DB-backed properties ride the integration lane; here sqlmock
// pins the SQL shape and the payload contract.
package taskio

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
	tcItem = "11111111-1111-1111-1111-111111111111"
	tcRun  = "22222222-2222-2222-2222-222222222222"
)

var tcTime = time.Unix(1700000000, 0).UTC()

// tcEchoArg matches the `ticket_comment` payload (arg $4 of
// CaptureRunForWorkItem's INSERT) against the durable comment it echoes.
type tcEchoArg struct {
	wantKind string
	body     string
}

func (a tcEchoArg) Match(v driver.Value) bool {
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
	return echo.WorkItemID == tcItem && echo.RunID == tcRun && echo.Author == "agent:sam" &&
		echo.Kind == a.wantKind && echo.Body == a.body && echo.At.Unix() == tcTime.Unix()
}

func newCommentTestStore(t *testing.T) (*CoordStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	state, err := coord.NewHumanStateStore(db)
	if err != nil {
		t.Fatalf("NewHumanStateStore: %v", err)
	}
	store, err := NewCoordStore(db, state)
	if err != nil {
		t.Fatalf("NewCoordStore: %v", err)
	}
	return store, mock
}

// TestPostCommentEchoesLive proves the plain post-comment captures the echo
// AFTER the durable append: the append query first, then the outbox insert on
// the pool (its own implicit transaction), keyed on the run.
func TestPostCommentEchoesLive(t *testing.T) {
	store, mock := newCommentTestStore(t)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.comment")).
		WithArgs(tcItem, "agent:sam", "the final word").
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(tcTime))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.outbox")).
		WithArgs(tcItem, coord.LiveCommentEventType, tcRun,
			tcEchoArg{wantKind: "", body: "the final word"}, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))

	c, err := store.PostComment(context.Background(), tcItem, "agent:sam", tcRun, "the final word")
	if err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	if c.Body != "the final word" || !c.CreatedAt.Equal(tcTime) {
		t.Fatalf("comment mismatched: %+v", c)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestPostSummaryEchoesLive proves the run's final structured summary (kind
// 'summary') also echoes — this is the frame that frees the ticket page from
// the ended-milestone race.
func TestPostSummaryEchoesLive(t *testing.T) {
	store, mock := newCommentTestStore(t)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.comment")).
		WithArgs(tcItem, "agent:sam", "What I did: shipped the seam", tcRun).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(tcTime))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.outbox")).
		WithArgs(tcItem, coord.LiveCommentEventType, tcRun,
			tcEchoArg{wantKind: "summary", body: "What I did: shipped the seam"}, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))

	c, err := store.PostSummary(context.Background(), tcItem, "agent:sam", tcRun, "What I did: shipped the seam")
	if err != nil {
		t.Fatalf("PostSummary: %v", err)
	}
	if c.Body != "What I did: shipped the seam" {
		t.Fatalf("summary mismatched: %+v", c)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestPostCommentWithoutRunSkipsEcho: no run correlation, nothing keys the
// per-run fan — the append commits and NO outbox insert is expected.
func TestPostCommentWithoutRunSkipsEcho(t *testing.T) {
	store, mock := newCommentTestStore(t)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.comment")).
		WithArgs(tcItem, "agent:sam", "offline note").
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(tcTime))

	if _, err := store.PostComment(context.Background(), tcItem, "agent:sam", "", "offline note"); err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestPostCommentEchoFailureNeverFailsTheVerb pins the best-effort posture: a
// broken outbox costs liveness only — the verb still returns the durable
// comment.
func TestPostCommentEchoFailureNeverFailsTheVerb(t *testing.T) {
	store, mock := newCommentTestStore(t)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.comment")).
		WithArgs(tcItem, "agent:sam", "still durable").
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(tcTime))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO coord.outbox")).
		WithArgs(tcItem, coord.LiveCommentEventType, tcRun, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(sqlmock.ErrCancelled)

	c, err := store.PostComment(context.Background(), tcItem, "agent:sam", tcRun, "still durable")
	if err != nil {
		t.Fatalf("PostComment must survive a lost echo: %v", err)
	}
	if !strings.Contains(c.Body, "still durable") {
		t.Fatalf("comment mismatched: %+v", c)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
