package discussion

// ISI-7272 (ISI-7262 WS-1): the store-level outbox capture for run-authored
// posts — sqlmock lane (the DB-backed happy paths live in the Postgres
// integration lane). Strict sqlmock ordering doubles as the negative proof:
// a human post runs NO claim/outbox query at all.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func outboxStrPtr(s string) *string { return &s }

// payloadMatcher matches the outbox INSERT's payload argument when it carries
// the expected message.created envelope for msgID.
type payloadMatcher struct{ msgID string }

func (m payloadMatcher) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	return strings.Contains(s, `"type":"message.created"`) && strings.Contains(s, m.msgID)
}

func (m payloadMatcher) String() string { return "payload containing message.created " + m.msgID }

func newOutboxMock(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db), mock
}

func expectThreadInScope(mock sqlmock.Sqlmock, threadID uuid.UUID, project string, team uuid.UUID) {
	mock.ExpectQuery("SELECT 1 FROM discussion.thread").
		WithArgs(threadID.String(), project, team.String()).
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
}

func expectMessageInsert(mock sqlmock.Sqlmock, msgID uuid.UUID) {
	mock.ExpectQuery("INSERT INTO discussion.message").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(msgID, time.Now()))
}

// A run-authored post resolves the run's claim → work item → project UID and
// captures a discussion_message run-entity outbox row whose payload is the
// message.created RoomEvent envelope.
func TestPostMessageRunAuthoredCapturesOutboxEvent(t *testing.T) {
	store, mock := newOutboxMock(t)
	team, threadID := uuid.New(), uuid.New()
	runID := uuid.New().String()
	auth := AuthorContext{Principal: "agent:alpha", TeamID: team, AgentID: outboxStrPtr(uuid.New().String()), RunID: outboxStrPtr(runID)}
	msgID := uuid.New()
	projectUID, workItem := uuid.New().String(), uuid.New().String()

	expectThreadInScope(mock, threadID, "ns/proj", team)
	expectMessageInsert(mock, msgID)
	mock.ExpectQuery("SELECT w.project_id::text, w.id::text").WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "work_item_id"}).AddRow(projectUID, workItem))
	mock.ExpectExec("INSERT INTO coord.outbox").
		WithArgs("run", projectUID, team.String(), MessageEventType, workItem, runID, payloadMatcher{msgID: msgID.String()}, sqlmock.AnyArg()).
		WillReturnResult(driver.RowsAffected(1))

	msg, err := store.PostMessage(context.Background(), "ns/proj", team, threadID, auth, "final answer", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if msg.ID != msgID {
		t.Fatalf("message id = %v, want %v", msg.ID, msgID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A human post (RunID nil) runs NO claim lookup and NO outbox insert — the
// handler's synchronous echo owns the live frame for humans.
func TestPostMessageHumanAuthoredDoesNotCapture(t *testing.T) {
	store, mock := newOutboxMock(t)
	team, threadID := uuid.New(), uuid.New()
	auth := AuthorContext{Principal: "user:henrik", TeamID: team}

	expectThreadInScope(mock, threadID, "ns/proj", team)
	expectMessageInsert(mock, uuid.New())

	if _, err := store.PostMessage(context.Background(), "ns/proj", team, threadID, auth, "hello", nil, nil, nil, nil); err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("human post must not touch the outbox: %v", err)
	}
}

// A run with no coord.claim row (ambient agent post) skips the capture —
// reload-only, exactly as before the seam — and still returns the message.
func TestCaptureSkipsWhenRunHasNoClaim(t *testing.T) {
	store, mock := newOutboxMock(t)
	team, threadID := uuid.New(), uuid.New()
	runID := uuid.New().String()
	auth := AuthorContext{Principal: "agent:alpha", TeamID: team, RunID: outboxStrPtr(runID)}

	expectThreadInScope(mock, threadID, "ns/proj", team)
	expectMessageInsert(mock, uuid.New())
	mock.ExpectQuery("SELECT w.project_id::text, w.id::text").WithArgs(runID).
		WillReturnError(sql.ErrNoRows)

	if _, err := store.PostMessage(context.Background(), "ns/proj", team, threadID, auth, "hi", nil, nil, nil, nil); err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no-claim must skip the outbox insert: %v", err)
	}
}

// The capture is best-effort: an outbox INSERT failure never fails the post.
func TestCaptureFailureNeverFailsPost(t *testing.T) {
	store, mock := newOutboxMock(t)
	team, threadID := uuid.New(), uuid.New()
	runID := uuid.New().String()
	auth := AuthorContext{Principal: "agent:alpha", TeamID: team, RunID: outboxStrPtr(runID)}
	msgID := uuid.New()

	expectThreadInScope(mock, threadID, "ns/proj", team)
	expectMessageInsert(mock, msgID)
	mock.ExpectQuery("SELECT w.project_id::text, w.id::text").WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "work_item_id"}).AddRow(uuid.New().String(), uuid.New().String()))
	mock.ExpectExec("INSERT INTO coord.outbox").
		WillReturnError(errors.New("outbox down"))

	msg, err := store.PostMessage(context.Background(), "ns/proj", team, threadID, auth, "hi", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("a capture failure must not fail the write: %v", err)
	}
	if msg.ID != msgID {
		t.Fatalf("message id = %v, want %v", msg.ID, msgID)
	}
}

// The envelope marshals the same wire shape the handler's synchronous echo
// (apiserver roomStreamPublisher) emits — {type,message} with the message's
// camelCase body — so the console parser treats both identically.
func TestRoomEventEnvelopeShape(t *testing.T) {
	mid := uuid.New()
	tid := uuid.New()
	body, err := json.Marshal(roomEventEnvelope{Type: "message.created", Message: &Message{ID: mid, ThreadID: tid, Body: "done"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(body)
	if !strings.HasPrefix(s, `{"type":"message.created","message":{`) {
		t.Fatalf("envelope prefix mismatch: %s", s)
	}
	for _, want := range []string{`"id":"` + mid.String() + `"`, `"threadId":"` + tid.String() + `"`, `"body":"done"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("envelope missing %s in %s", want, s)
		}
	}
}
