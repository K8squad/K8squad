package discussion

// ISI-7272 (ISI-7262 WS-1): the synchronous room echo covers HUMAN posts
// only — a run-authored post relies on the store's outbox capture
// (outbox.go) bridged by the apiserver projector, so the handler must not
// double-echo it. Both lanes share the same happy-path sqlmock script; the
// run lane additionally expects the claim/outbox queries.

import (
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// recordingRoomStream records synchronous echoes (fake RoomStreamPublisher).
type recordingRoomStream struct{ created []*Message }

func (r *recordingRoomStream) PublishMessageCreated(_ string, msg *Message) {
	r.created = append(r.created, msg)
}

func (r *recordingRoomStream) PublishMessageDeleted(_ string, _ string) {}

func newPostMessageHTTPRequest(t *testing.T, auth AuthorContext, project string, thread uuid.UUID) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"body":"hello"}`))
	req = mux.SetURLVars(req, map[string]string{"projectId": project, "threadId": thread.String()})
	return req.WithContext(WithAuth(req.Context(), auth))
}

func TestPostMessageEchoGatedOnRunAuthorship(t *testing.T) {
	team, threadID, msgID := uuid.New(), uuid.New(), uuid.New()
	runID := uuid.New().String()
	runAuth := AuthorContext{Principal: "agent:alpha", TeamID: team, AgentID: outboxStrPtr(uuid.New().String()), RunID: outboxStrPtr(runID)}
	humanAuth := AuthorContext{Principal: "user:henrik", TeamID: team}

	newCase := func(t *testing.T, auth AuthorContext) (*Handler, sqlmock.Sqlmock, *recordingRoomStream) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		mock.ExpectQuery("SELECT 1 FROM discussion.thread").
			WithArgs(threadID.String(), "ns/proj", team.String()).
			WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
		mock.ExpectQuery("INSERT INTO discussion.message").
			WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(msgID, time.Now()))
		if auth.RunID != nil {
			mock.ExpectQuery("SELECT w.project_id::text, w.id::text").
				WillReturnRows(sqlmock.NewRows([]string{"project_id", "work_item_id"}).AddRow(uuid.New().String(), uuid.New().String()))
			mock.ExpectExec("INSERT INTO coord.outbox").
				WillReturnResult(driver.RowsAffected(1))
		}
		h := NewHandler(NewStore(db))
		stream := &recordingRoomStream{}
		h.SetRoomStreamPublisher(stream)
		return h, mock, stream
	}

	t.Run("human post echoes synchronously", func(t *testing.T) {
		h, mock, stream := newCase(t, humanAuth)
		rec := httptest.NewRecorder()
		h.postMessage(rec, newPostMessageHTTPRequest(t, humanAuth, "ns/proj", threadID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rec.Code)
		}
		if len(stream.created) != 1 {
			t.Fatalf("echoes = %d, want 1", len(stream.created))
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("run-authored post leaves the echo to the outbox bridge", func(t *testing.T) {
		h, mock, stream := newCase(t, runAuth)
		rec := httptest.NewRecorder()
		h.postMessage(rec, newPostMessageHTTPRequest(t, runAuth, "ns/proj", threadID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rec.Code)
		}
		if len(stream.created) != 0 {
			t.Fatalf("echoes = %d, want 0 (the outbox bridge owns the live frame)", len(stream.created))
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}
