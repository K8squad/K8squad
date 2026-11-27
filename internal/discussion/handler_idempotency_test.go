package discussion

// ISI-7907 (ISI-7893 Layer 4): a client-supplied Idempotency-Key collapses a
// duplicate POST /messages — a double-clicked composer, an at-least-once retry — to
// ONE stored message within a short window. The replay runs NO second INSERT (proven
// by the sqlmock issuing zero queries on the second call) and NO second SSE echo, and
// returns the original message flagged X-Idempotent-Replay.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

func newPostMessageReqWithKey(auth AuthorContext, project string, thread uuid.UUID, idemKey string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"body":"hello"}`))
	req = mux.SetURLVars(req, map[string]string{"projectId": project, "threadId": thread.String()})
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return req.WithContext(WithAuth(req.Context(), auth))
}

func TestPostMessageIdempotentReplay(t *testing.T) {
	team, threadID, msgID := uuid.New(), uuid.New(), uuid.New()
	humanAuth := AuthorContext{Principal: "user:henrik", TeamID: team}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Exactly ONE write cycle is scripted: the thread-scope check + the message
	// INSERT. The second POST must not consume any expectation — the cache replays.
	mock.ExpectQuery("SELECT 1 FROM discussion.thread").
		WithArgs(threadID.String(), "ns/proj", team.String()).
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	mock.ExpectQuery("INSERT INTO discussion.message").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(msgID, time.Now()))

	h := NewHandler(NewStore(db))
	stream := &recordingRoomStream{}
	h.SetRoomStreamPublisher(stream)

	// First POST: writes + echoes once.
	rec1 := httptest.NewRecorder()
	h.postMessage(rec1, newPostMessageReqWithKey(humanAuth, "ns/proj", threadID, "dup-1"))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201", rec1.Code)
	}
	if rec1.Header().Get("X-Idempotent-Replay") != "" {
		t.Fatal("first POST must not be flagged as a replay")
	}

	// Replay POST: same key → original message, no DB query, no second echo.
	rec2 := httptest.NewRecorder()
	h.postMessage(rec2, newPostMessageReqWithKey(humanAuth, "ns/proj", threadID, "dup-1"))
	if rec2.Code != http.StatusCreated {
		t.Fatalf("replay POST: status = %d, want 201", rec2.Code)
	}
	if rec2.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatal("replay POST must set X-Idempotent-Replay: true")
	}

	if len(stream.created) != 1 {
		t.Fatalf("echoes = %d, want 1 (the replay must not re-echo)", len(stream.created))
	}
	if rec1.Body.String() != rec2.Body.String() {
		t.Fatalf("replay body differs:\n first=%s\n replay=%s", rec1.Body.String(), rec2.Body.String())
	}
	// ExpectationsWereMet confirms the replay issued ZERO queries: only the single
	// scripted write cycle ran.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("replay must not touch the DB: %v", err)
	}
}

// A different key is a distinct intent: it runs a second full write cycle.
func TestPostMessageDistinctKeysBothWrite(t *testing.T) {
	team, threadID := uuid.New(), uuid.New()
	humanAuth := AuthorContext{Principal: "user:henrik", TeamID: team}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for i := 0; i < 2; i++ {
		mock.ExpectQuery("SELECT 1 FROM discussion.thread").
			WithArgs(threadID.String(), "ns/proj", team.String()).
			WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
		mock.ExpectQuery("INSERT INTO discussion.message").
			WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(uuid.New(), time.Now()))
	}

	h := NewHandler(NewStore(db))
	h.SetRoomStreamPublisher(&recordingRoomStream{})

	for _, k := range []string{"key-a", "key-b"} {
		rec := httptest.NewRecorder()
		h.postMessage(rec, newPostMessageReqWithKey(humanAuth, "ns/proj", threadID, k))
		if rec.Code != http.StatusCreated {
			t.Fatalf("key %s: status = %d, want 201", k, rec.Code)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("distinct keys must each write: %v", err)
	}
}

// An overlong key is rejected 400 before any DB work (bounded-memory guard).
func TestPostMessageOverlongKey(t *testing.T) {
	team, threadID := uuid.New(), uuid.New()
	humanAuth := AuthorContext{Principal: "user:henrik", TeamID: team}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// No expectations: the handler must reject before touching the store.

	h := NewHandler(NewStore(db))
	rec := httptest.NewRecorder()
	h.postMessage(rec, newPostMessageReqWithKey(humanAuth, "ns/proj", threadID, strings.Repeat("k", 256)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overlong key: status = %d, want 400", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("overlong key must not touch the DB: %v", err)
	}
}
