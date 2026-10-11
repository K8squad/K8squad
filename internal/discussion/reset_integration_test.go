//go:build discussion_integration

// DB-backed coverage for the ISI-5672 /reset backend (ADR-0030 Option 2 "reset-marker") against a REAL
// Postgres with the shipped migration chain (incl. 0036). Build-tag gated to the integration lane; it
// reuses openPartyTestDB (which applies 0033 party_session + 0036 reset), so the party-session-close
// ruling (§(b)) is exercised against the real table, not a mock. SKIPS when DATABASE_URL is unset.
//
//	go test -tags=discussion_integration ./internal/discussion/...
package discussion

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// tick nudges the wall clock so a subsequent auto-commit statement gets a strictly-later now()
// transaction timestamp — the reset floor is a STRICT created_at > reset_at boundary (ADR-0030 §(c)),
// and this keeps the ordering deterministic rather than relying on sub-microsecond round-trip timing.
func tick() { time.Sleep(3 * time.Millisecond) }

func postText(t *testing.T, store *Store, projectID string, teamID, threadID uuid.UUID, auth AuthorContext, body string) *Message {
	t.Helper()
	m, err := store.PostMessage(context.Background(), projectID, teamID, threadID, auth, body, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("PostMessage(%q): %v", body, err)
	}
	return m
}

func bodies(msgs []Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Body)
	}
	return out
}

// TestResetThread_HidesPriorShowsPost is the core reader-filter contract: after /reset the visible
// transcript is empty, the marker is NOT a bubble, reset metadata is exposed, and messages posted AFTER
// the reset appear while pre-reset ones stay hidden.
func TestResetThread_HidesPriorShowsPost(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "ns/reset-proj"
	teamID := uuid.New()
	human := AuthorContext{Principal: "user:alice", TeamID: teamID}

	thread, err := store.OpenThread(ctx, projectID, human, "reset thread", "first message")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	postText(t, store, projectID, teamID, thread.ID, human, "second message")
	postText(t, store, projectID, teamID, thread.ID, human, "third message")

	// Before reset: all three visible, no reset metadata.
	got, err := store.GetThread(ctx, projectID, teamID, thread.ID)
	if err != nil {
		t.Fatalf("GetThread (pre-reset): %v", err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("pre-reset visible = %v, want 3", bodies(got.Messages))
	}
	if got.ResetAt != nil {
		t.Fatalf("pre-reset ResetAt = %v, want nil", got.ResetAt)
	}

	tick()
	marker, hidden, err := store.ResetThread(ctx, projectID, teamID, thread.ID, human, "starting fresh")
	if err != nil {
		t.Fatalf("ResetThread: %v", err)
	}
	if hidden != 3 {
		t.Fatalf("hiddenCount = %d, want 3", hidden)
	}
	if marker.Kind != "reset" {
		t.Fatalf("marker kind = %q, want reset", marker.Kind)
	}
	if marker.AuthorAgentID != nil {
		t.Fatalf("marker author_agent_id = %v, want NULL (human-only fence §(a)7)", *marker.AuthorAgentID)
	}

	// Immediately after reset: empty transcript, marker NOT shown, metadata populated.
	got, err = store.GetThread(ctx, projectID, teamID, thread.ID)
	if err != nil {
		t.Fatalf("GetThread (post-reset): %v", err)
	}
	if len(got.Messages) != 0 {
		t.Fatalf("post-reset visible = %v, want empty (marker excluded, prior hidden)", bodies(got.Messages))
	}
	if got.ResetAt == nil || got.ResetBy == nil {
		t.Fatalf("post-reset metadata = (%v,%v), want both set", got.ResetAt, got.ResetBy)
	}
	if *got.ResetBy != "user:alice" {
		t.Fatalf("ResetBy = %q, want user:alice", *got.ResetBy)
	}
	if got.ResetReason == nil || *got.ResetReason != "starting fresh" {
		t.Fatalf("ResetReason = %v, want %q", got.ResetReason, "starting fresh")
	}

	// A message posted AFTER the reset reappears; the pre-reset ones stay hidden.
	tick()
	postText(t, store, projectID, teamID, thread.ID, human, "after reset")
	got, err = store.GetThread(ctx, projectID, teamID, thread.ID)
	if err != nil {
		t.Fatalf("GetThread (after new post): %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Body != "after reset" {
		t.Fatalf("post-reset-new visible = %v, want [after reset]", bodies(got.Messages))
	}
}

// TestResetThread_RetractMarkerUnResets: retracting the reset marker drops it from the live-marker set,
// the floor falls away, and the hidden transcript reappears — the undo-reset path, no new write.
func TestResetThread_RetractMarkerUnResets(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "ns/unreset-proj"
	teamID := uuid.New()
	human := AuthorContext{Principal: "user:bob", TeamID: teamID}

	thread, err := store.OpenThread(ctx, projectID, human, "unreset thread", "one")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	postText(t, store, projectID, teamID, thread.ID, human, "two")

	tick()
	marker, _, err := store.ResetThread(ctx, projectID, teamID, thread.ID, human, "")
	if err != nil {
		t.Fatalf("ResetThread: %v", err)
	}
	got, _ := store.GetThread(ctx, projectID, teamID, thread.ID)
	if len(got.Messages) != 0 {
		t.Fatalf("after reset visible = %v, want empty", bodies(got.Messages))
	}

	// Retract the marker (author-or-admin path) → un-reset.
	if err := store.Retract(ctx, projectID, teamID, thread.ID, marker.ID, human); err != nil {
		t.Fatalf("Retract marker: %v", err)
	}
	got, err = store.GetThread(ctx, projectID, teamID, thread.ID)
	if err != nil {
		t.Fatalf("GetThread (post-unreset): %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("after un-reset visible = %v, want [one two]", bodies(got.Messages))
	}
	if got.ResetAt != nil {
		t.Fatalf("after un-reset ResetAt = %v, want nil", got.ResetAt)
	}
}

// TestResetThread_MultipleResetsLatestWins: the floor is the LATEST live marker, so a message between
// two resets is hidden by the second, and only post-second-reset content is visible.
func TestResetThread_MultipleResetsLatestWins(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "ns/multi-reset"
	teamID := uuid.New()
	human := AuthorContext{Principal: "user:carol", TeamID: teamID}

	thread, err := store.OpenThread(ctx, projectID, human, "multi", "orig")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	tick()
	if _, _, err := store.ResetThread(ctx, projectID, teamID, thread.ID, human, "first"); err != nil {
		t.Fatalf("ResetThread #1: %v", err)
	}
	tick()
	postText(t, store, projectID, teamID, thread.ID, human, "between resets")
	tick()
	if _, _, err := store.ResetThread(ctx, projectID, teamID, thread.ID, human, "second"); err != nil {
		t.Fatalf("ResetThread #2: %v", err)
	}
	tick()
	postText(t, store, projectID, teamID, thread.ID, human, "after second")

	got, err := store.GetThread(ctx, projectID, teamID, thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Body != "after second" {
		t.Fatalf("latest-wins visible = %v, want [after second]", bodies(got.Messages))
	}
	if got.ResetReason == nil || *got.ResetReason != "second" {
		t.Fatalf("ResetReason = %v, want latest marker reason %q", got.ResetReason, "second")
	}
}

// TestResetThread_EmptyThreadReset: resetting a thread with only its opening message is legal and hides
// that one message (hiddenCount=1); the thread is now empty but still a valid, resettable thread.
func TestResetThread_EmptyThreadReset(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "ns/empty-reset"
	teamID := uuid.New()
	human := AuthorContext{Principal: "user:dan", TeamID: teamID}

	thread, err := store.OpenThread(ctx, projectID, human, "lonely", "only message")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	tick()
	_, hidden, err := store.ResetThread(ctx, projectID, teamID, thread.ID, human, "")
	if err != nil {
		t.Fatalf("ResetThread: %v", err)
	}
	if hidden != 1 {
		t.Fatalf("hiddenCount = %d, want 1", hidden)
	}
	got, _ := store.GetThread(ctx, projectID, teamID, thread.ID)
	if len(got.Messages) != 0 {
		t.Fatalf("visible = %v, want empty", bodies(got.Messages))
	}
}

// TestResetThread_AgentRejected: an agent principal cannot reset (human-only fence §(a)7), and nothing
// is written.
func TestResetThread_AgentRejected(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "ns/agent-reset"
	teamID := uuid.New()
	human := AuthorContext{Principal: "user:ed", TeamID: teamID}
	thread, err := store.OpenThread(ctx, projectID, human, "agent", "msg")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}

	agentID := "amelia"
	runID := "run-9"
	bot := AuthorContext{Principal: "agent:amelia", TeamID: teamID, AgentID: &agentID, RunID: &runID}
	if _, _, err := store.ResetThread(ctx, projectID, teamID, thread.ID, bot, "nope"); err != ErrResetForbidden {
		t.Fatalf("ResetThread(agent) = %v, want ErrResetForbidden", err)
	}
	// No marker written: the thread still shows its original message, no reset metadata.
	got, _ := store.GetThread(ctx, projectID, teamID, thread.ID)
	if len(got.Messages) != 1 || got.ResetAt != nil {
		t.Fatalf("after rejected reset: visible=%v resetAt=%v, want [msg] and nil", bodies(got.Messages), got.ResetAt)
	}
}

// TestResetThread_ClosesActivePartySession (§(b)): a /reset atomically closes any active party-session
// in the thread, so no advancer keeps minting paid Runs against a now-hidden topic.
func TestResetThread_ClosesActivePartySession(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "ns/party-reset"
	teamID := uuid.New()
	human := AuthorContext{Principal: "user:fay", TeamID: teamID}

	thread, err := store.OpenThread(ctx, projectID, human, "party", "topic")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	budget := DefaultPartyBudget()
	sess, _, err := store.StartPartySession(ctx, projectID, teamID, thread.ID, human, "let's debate", &budget)
	if err != nil {
		t.Fatalf("StartPartySession: %v", err)
	}
	if !sess.IsActive() {
		t.Fatalf("session phase = %q, want active", sess.Phase)
	}

	tick()
	if _, _, err := store.ResetThread(ctx, projectID, teamID, thread.ID, human, "clear"); err != nil {
		t.Fatalf("ResetThread: %v", err)
	}

	// The active session is gone (closed), so the advancer's active lookup misses.
	if _, err := store.ActivePartySession(ctx, thread.ID); err != ErrNoActivePartySession {
		t.Fatalf("ActivePartySession after reset = %v, want ErrNoActivePartySession", err)
	}
	closed, err := store.GetPartySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetPartySession: %v", err)
	}
	if closed.Phase != PartyPhaseClosed {
		t.Fatalf("session phase after reset = %q, want closed", closed.Phase)
	}
	if closed.ClosedAt == nil {
		t.Fatal("session closed_at after reset = nil, want set")
	}
}

// TestResetThread_OrphanPromotion (§(c)): a reply whose parent is below the reset floor (hidden) is
// PROMOTED to a root rather than dropped, so a straddling reply never silently vanishes.
func TestResetThread_OrphanPromotion(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "ns/orphan-reset"
	teamID := uuid.New()
	human := AuthorContext{Principal: "user:gus", TeamID: teamID}

	thread, err := store.OpenThread(ctx, projectID, human, "orphan", "parent message")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	parent := thread.Messages[0] // the opening message becomes the (soon-hidden) parent

	tick()
	if _, _, err := store.ResetThread(ctx, projectID, teamID, thread.ID, human, ""); err != nil {
		t.Fatalf("ResetThread: %v", err)
	}
	// A reply to the now-hidden parent, posted AFTER the reset (straddles the floor).
	tick()
	reply, err := store.PostMessage(ctx, projectID, teamID, thread.ID, human, "reply to hidden parent", &parent.ID, nil, nil, nil)
	if err != nil {
		t.Fatalf("PostMessage reply: %v", err)
	}

	got, err := store.GetThread(ctx, projectID, teamID, thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	// The reply must appear as a ROOT (its parent is hidden, not in the visible set), not be dropped.
	if len(got.Messages) != 1 || got.Messages[0].ID != reply.ID {
		t.Fatalf("orphan-promotion visible = %v, want the straddling reply promoted to a root", bodies(got.Messages))
	}
}

// TestResetThread_ListThreadsMarkerInclusive (§(c)/§(d)): a freshly-reset thread stays in ListThreads
// (its marker is a live message) and its LastActivityAt reflects the marker (marker-inclusive activity),
// so it sorts as recently-touched even with an empty visible preview.
func TestResetThread_ListThreadsMarkerInclusive(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "ns/list-reset"
	teamID := uuid.New()
	human := AuthorContext{Principal: "user:hal", TeamID: teamID}

	thread, err := store.OpenThread(ctx, projectID, human, "listed", "msg")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	tick()
	marker, _, err := store.ResetThread(ctx, projectID, teamID, thread.ID, human, "")
	if err != nil {
		t.Fatalf("ResetThread: %v", err)
	}

	threads, err := store.ListThreads(ctx, projectID, teamID, 50, 0)
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("ListThreads len = %d, want 1 (reset thread still listed via its live marker)", len(threads))
	}
	// LastActivityAt is marker-inclusive: it tracks the reset marker's created_at, not the pre-reset msg.
	if !threads[0].LastActivityAt.Equal(marker.CreatedAt) {
		t.Fatalf("LastActivityAt = %v, want the marker's created_at %v (marker-inclusive)", threads[0].LastActivityAt, marker.CreatedAt)
	}
}
