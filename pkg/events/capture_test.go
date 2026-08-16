package events

import (
	"context"
	"database/sql"
	"testing"
)

// recExecer records ExecContext calls. It is ONLY an Execer — it has no Commit
// or Rollback — which is the point of the C1 primitive: Capture operates purely
// on the handle it is handed and never owns the transaction boundary. In
// production that handle is the caller's in-flight *sql.Tx, so the event append
// and the state change commit or roll back together.
type recExecer struct {
	queries []string
	args    [][]any
}

type nopResult struct{}

func (nopResult) LastInsertId() (int64, error) { return 0, nil }
func (nopResult) RowsAffected() (int64, error) { return 1, nil }

func (r *recExecer) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	r.queries = append(r.queries, q)
	r.args = append(r.args, args)
	return nopResult{}, nil
}

func TestCapture_ValidEventInsertsOnce(t *testing.T) {
	ex := &recExecer{}
	ev := Event{Entity: "work_item", ProjectID: "p1", Squad: "s1", EventType: "claimed",
		WorkItemID: "w1", RunID: "r1", Payload: []byte(`{"v":1}`)}
	if err := Capture(context.Background(), ex, ev); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(ex.queries) != 1 {
		t.Fatalf("expected exactly one INSERT, got %d", len(ex.queries))
	}
	args := ex.args[0]
	if args[0] != "work_item" || args[1] != "p1" || args[2] != "s1" || args[3] != "claimed" {
		t.Fatalf("taxonomy args wrong: %v", args[:4])
	}
	if args[6] != `{"v":1}` {
		t.Fatalf("payload arg = %v, want passthrough", args[6])
	}
}

func TestCapture_NilPayloadDefaultsToEmptyObject(t *testing.T) {
	ex := &recExecer{}
	if err := Capture(context.Background(), ex, Event{Entity: "run", ProjectID: "p", EventType: "started"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got := ex.args[0][6]; got != "{}" {
		t.Fatalf("nil payload stored as %v, want %q", got, "{}")
	}
}

func TestCapture_EmptySquadStoredAsNull(t *testing.T) {
	ex := &recExecer{}
	if err := Capture(context.Background(), ex, Event{Entity: "run", ProjectID: "p", EventType: "started"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got := ex.args[0][2]; got != nil {
		t.Fatalf("empty squad stored as %v, want SQL NULL (nil)", got)
	}
}

func TestCapture_RejectsBadEntityBeforeDB(t *testing.T) {
	ex := &recExecer{}
	err := Capture(context.Background(), ex, Event{Entity: "bogus", ProjectID: "p", EventType: "x"})
	if err == nil {
		t.Fatal("expected error for invalid entity")
	}
	if len(ex.queries) != 0 {
		t.Fatal("invalid entity must fail fast, before touching the DB")
	}
}

func TestCapture_RequiresProjectAndEventType(t *testing.T) {
	ex := &recExecer{}
	if err := Capture(context.Background(), ex, Event{Entity: "run", EventType: "x"}); err == nil {
		t.Fatal("expected error for missing project_id")
	}
	if err := Capture(context.Background(), ex, Event{Entity: "run", ProjectID: "p"}); err == nil {
		t.Fatal("expected error for missing event_type")
	}
}
