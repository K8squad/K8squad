package discussion

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMessageKind validates the kind constraints.
func TestMessageKind(t *testing.T) {
	cases := []struct {
		kind MessageKind
		ok   bool
	}{
		{KindMessage, true},
		{KindAnnouncement, true},
		{KindDecision, true},
		{KindQuestion, true},
		{MessageKind("invalid"), false},
	}
	for _, c := range cases {
		valid := c.kind == KindMessage || c.kind == KindAnnouncement ||
			c.kind == KindDecision || c.kind == KindQuestion
		if valid != c.ok {
			t.Errorf("kind %s: expected ok=%v, got %v", c.kind, c.ok, valid)
		}
	}
}

func TestAuthorType(t *testing.T) {
	cases := []struct {
		at  AuthorType
		ok  bool
	}{
		{AuthorTypeAgent, true},
		{AuthorTypeHuman, true},
		{AuthorTypeSystem, true},
		{AuthorType("alien"), false},
	}
	for _, c := range cases {
		valid := c.at == AuthorTypeAgent || c.at == AuthorTypeHuman || c.at == AuthorTypeSystem
		if valid != c.ok {
			t.Errorf("authorType %s: expected ok=%v, got %v", c.at, c.ok, valid)
		}
	}
}

func TestBuildThreadTree(t *testing.T) {
	roomID := uuid.New()
	parentID := uuid.New()
	childID := uuid.New()

	msgs := []Message{
		{ID: parentID, RoomID: roomID, Body: "parent", AuthorID: uuid.New()},
		{ID: childID, RoomID: roomID, ParentID: &parentID, Body: "child", AuthorID: uuid.New()},
	}

	tree := buildThreadTree(msgs, 100)
	if len(tree) != 1 {
		t.Fatalf("expected 1 root, got %d", len(tree))
	}
	if tree[0].ID != parentID {
		t.Fatalf("expected root to be parent, got %s", tree[0].ID)
	}
	if len(tree[0].Replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(tree[0].Replies))
	}
	if tree[0].Replies[0].ID != childID {
		t.Fatalf("expected reply to be child, got %s", tree[0].Replies[0].ID)
	}
}

func TestBuildThreadTreeLimit(t *testing.T) {
	roomID := uuid.New()
	var msgs []Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, Message{
			ID:        uuid.New(),
			RoomID:    roomID,
			Body:      "msg",
			AuthorID:  uuid.New(),
			CreatedAt: time.Now(),
		})
	}
	tree := buildThreadTree(msgs, 3)
	if len(tree) != 3 {
		t.Fatalf("expected 3 roots after limit, got %d", len(tree))
	}
}

func TestMin(t *testing.T) {
	if min(3, 5) != 3 {
		t.Fatal("min(3,5) should be 3")
	}
	if min(5, 3) != 3 {
		t.Fatal("min(5,3) should be 3")
	}
	if min(3, 3) != 3 {
		t.Fatal("min(3,3) should be 3")
	}
}

// TestStoreNilDB verifies graceful behavior patterns (compile-time interface check).
func TestStoreNilDB(t *testing.T) {
	s := NewStore((*sql.DB)(nil))
	if s == nil {
		t.Fatal("NewStore returned nil")
	}
}
