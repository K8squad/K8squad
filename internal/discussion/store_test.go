package discussion

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAuthorKindDerived confirms agent-vs-human is DERIVED from author_agent_id, never a stored flag
// (§7.5 / 10.3 badge). author_agent_id present ⇒ agent; absent ⇒ human.
func TestAuthorKindDerived(t *testing.T) {
	agentID := "agent:coordinator"
	if got := (Message{AuthorAgentID: &agentID}).AuthorKind(); got != "agent" {
		t.Errorf("author_agent_id set: got %q, want agent", got)
	}
	if got := (Message{AuthorAgentID: nil}).AuthorKind(); got != "human" {
		t.Errorf("author_agent_id nil: got %q, want human", got)
	}
}

// TestRetracted confirms the soft-retract predicate (§7.4).
func TestRetracted(t *testing.T) {
	if (Message{}).Retracted() {
		t.Error("a message with nil invalidated_at must not be Retracted")
	}
	now := time.Now()
	if !(Message{InvalidatedAt: &now}).Retracted() {
		t.Error("a message with invalidated_at set must be Retracted")
	}
}

// TestBuildThreadTree nests replies under parents via parent_id adjacency, preserving created_at order.
func TestBuildThreadTree(t *testing.T) {
	parentID := uuid.New()
	childID := uuid.New()
	grandID := uuid.New()
	msgs := []Message{
		{ID: parentID, Body: "root"},
		{ID: childID, ParentID: &parentID, Body: "reply"},
		{ID: grandID, ParentID: &childID, Body: "reply-to-reply"},
	}
	tree := buildThreadTree(msgs)
	if len(tree) != 1 {
		t.Fatalf("expected 1 root, got %d", len(tree))
	}
	if tree[0].ID != parentID {
		t.Fatalf("root should be parent, got %s", tree[0].ID)
	}
	if len(tree[0].Replies) != 1 || tree[0].Replies[0].ID != childID {
		t.Fatalf("expected child under root")
	}
	if len(tree[0].Replies[0].Replies) != 1 || tree[0].Replies[0].Replies[0].ID != grandID {
		t.Fatalf("expected grandchild nested arbitrary-depth")
	}
}

// TestBuildThreadTreeOrphanSurfacesAsRoot — a reply whose parent is absent (e.g. the parent was
// retracted and filtered out) must surface as a root, never be silently dropped.
func TestBuildThreadTreeOrphanSurfacesAsRoot(t *testing.T) {
	missingParent := uuid.New()
	orphanID := uuid.New()
	msgs := []Message{
		{ID: orphanID, ParentID: &missingParent, Body: "orphaned reply"},
	}
	tree := buildThreadTree(msgs)
	if len(tree) != 1 || tree[0].ID != orphanID {
		t.Fatalf("orphaned reply must surface as a root, got %+v", tree)
	}
}

// TestAuthorContextNullMapping confirms the optional provenance fields map to SQL NULL when unset and
// to a valid value when set — the seam that keeps author_agent_id / author_run_id nullable (R2).
func TestAuthorContextNullMapping(t *testing.T) {
	var a AuthorContext
	if a.agentID().Valid || a.runID().Valid {
		t.Error("unset AgentID/RunID must map to SQL NULL")
	}
	agent, run := "agent:x", "run:y"
	a = AuthorContext{AgentID: &agent, RunID: &run}
	if !a.agentID().Valid || a.agentID().String != agent {
		t.Error("set AgentID must map to a valid NullString")
	}
	if !a.runID().Valid || a.runID().String != run {
		t.Error("set RunID must map to a valid NullString")
	}
}
