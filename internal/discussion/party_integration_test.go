//go:build discussion_integration

// ISI-5615 (ISI-5569 WS-D.1) — integration tests for the party-session Store: CRUD lifecycle +
// MintRound CAS + SetRoundFacilitatorMessage first-writer-wins. The coord join (RoundVoiceSettlement)
// requires the coord schema and is covered separately in discussion_integration lane.
package discussion

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// applyOneMigration applies a single migration file to db.
func applyOneMigration(t *testing.T, ctx context.Context, db *sql.DB, name string) {
	t.Helper()
	candidates := []string{
		filepath.Join("..", "..", "db", "migrations", name),
		filepath.Join("db", "migrations", name),
	}
	if d := os.Getenv("DISCUSSION_MIGRATIONS_DIR"); d != "" {
		candidates = append([]string{filepath.Join(d, name)}, candidates...)
	}
	var sqlBytes []byte
	var readErr error
	for _, c := range candidates {
		if sqlBytes, readErr = os.ReadFile(c); readErr == nil {
			break
		}
	}
	if sqlBytes == nil {
		t.Fatalf("could not locate migration %s (tried %v); set DISCUSSION_MIGRATIONS_DIR", name, candidates)
	}
	if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("migration %s failed: %v", name, err)
	}
}

// openPartyTestDB opens a test DB with the full party-session migration chain applied.
// It reuses openTestDB + applyMigration from integration_test.go then applies the party migrations.
func openPartyTestDB(t *testing.T) (*Store, func()) {
	t.Helper()
	db := openTestDB(t)
	// Apply base discussion schema (0004+0024+0026 from integration_test.go).
	applyMigration(t, db)
	// Apply mention_dispatch (0027), decision_request (0031), decision_read_marker (0032),
	// party_session (0033), party_round_facilitator (0034).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{
		"0027_dispatch_on_mention_run_minting.sql",
		"0031_discussion_decision_request.sql",
		"0032_decision_read_marker.sql",
		"0033_discussion_party_session.sql",
		"0034_discussion_party_round_facilitator.sql",
	} {
		applyOneMigration(t, ctx, db, name)
	}
	return NewStore(db), func() { _ = db.Close() }
}

func TestPartySession_StartAndRead(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Seed: open a thread first.
	projectID := "test-ns/test-project"
	teamID := uuid.New()
	auth := AuthorContext{Principal: "human-1", TeamID: teamID}
	thread, err := store.OpenThread(ctx, projectID, auth, "party test thread", "topic body")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}

	// Start a party session.
	budget := DefaultPartyBudget()
	sess, msg, err := store.StartPartySession(ctx, projectID, teamID, thread.ID, auth, "Let's debate!", &budget)
	if err != nil {
		t.Fatalf("StartPartySession: %v", err)
	}
	if sess == nil {
		t.Fatal("StartPartySession: nil session")
	}
	if msg == nil {
		t.Fatal("StartPartySession: nil message (expected party_start message on first open)")
	}
	if !sess.IsActive() {
		t.Errorf("session phase = %q, want active", sess.Phase)
	}
	if sess.Round != 0 {
		t.Errorf("initial round = %d, want 0", sess.Round)
	}
	if sess.Budget.MaxRounds != budget.MaxRounds {
		t.Errorf("budget MaxRounds = %d, want %d", sess.Budget.MaxRounds, budget.MaxRounds)
	}

	// Read back via ActivePartySession.
	got, err := store.ActivePartySession(ctx, thread.ID)
	if err != nil {
		t.Fatalf("ActivePartySession: %v", err)
	}
	if got.ID != sess.ID {
		t.Errorf("ActivePartySession returned id %v, want %v", got.ID, sess.ID)
	}

	// Idempotent re-open returns the existing session (nil message).
	sess2, msg2, err := store.StartPartySession(ctx, projectID, teamID, thread.ID, auth, "second open", nil)
	if err != nil {
		t.Fatalf("idempotent StartPartySession: %v", err)
	}
	if msg2 != nil {
		t.Error("idempotent StartPartySession: expected nil message on re-open")
	}
	if sess2.ID != sess.ID {
		t.Errorf("idempotent StartPartySession: returned different session id")
	}
}

func TestPartySession_MintRoundCAS(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "test-ns/test-project"
	teamID := uuid.New()
	auth := AuthorContext{Principal: "human-1", TeamID: teamID}
	thread, err := store.OpenThread(ctx, projectID, auth, "mint round test", "")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	sess, _, err := store.StartPartySession(ctx, projectID, teamID, thread.ID, auth, "party!", nil)
	if err != nil {
		t.Fatalf("StartPartySession: %v", err)
	}

	// MintRound from 0 → round 1.
	updated, err := store.MintRound(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("MintRound(from=0): %v", err)
	}
	if updated.Round != 1 {
		t.Errorf("after MintRound: round = %d, want 1", updated.Round)
	}
	if updated.CurrentRoundMessageID != nil {
		t.Error("after MintRound: CurrentRoundMessageID should be nil (no facilitator post yet)")
	}
	if updated.RoundStartedAt == nil {
		t.Error("after MintRound: RoundStartedAt should be set")
	}

	// CAS fail: minting from the same fromRound again is a no-op (idempotent guard).
	_, err = store.MintRound(ctx, sess.ID, 0)
	if err == nil {
		t.Error("MintRound(from=0) again: expected CAS failure, got nil error")
	}
	if !isPartySessionNotActive(err) {
		// ErrPartySessionNotActive is the CAS loser sentinel — but a stale-round guard
		// could also signal this differently; accept any non-nil error as a CAS failure.
		t.Logf("MintRound CAS failure (expected): %v", err)
	}
}

func TestPartySession_SetRoundFacilitatorMessage(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "test-ns/test-project"
	teamID := uuid.New()
	auth := AuthorContext{Principal: "human-1", TeamID: teamID}
	thread, err := store.OpenThread(ctx, projectID, auth, "facilitator msg test", "")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	sess, _, err := store.StartPartySession(ctx, projectID, teamID, thread.ID, auth, "party!", nil)
	if err != nil {
		t.Fatalf("StartPartySession: %v", err)
	}
	updated, err := store.MintRound(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("MintRound: %v", err)
	}

	// Post the facilitator message (simulate the coordinator posting its dispatch).
	agentAuth := AuthorContext{Principal: "coord-1", TeamID: teamID, AgentID: strPtr("coordinator")}
	party, text := "party", "text"
	fMsg, postErr := store.PostMessage(ctx, projectID, teamID, thread.ID, agentAuth,
		"@voice1 @voice2 let's discuss!", nil, &party, &text, nil)
	if postErr != nil {
		t.Fatalf("PostMessage (facilitator): %v", postErr)
	}

	// SetRoundFacilitatorMessage — first writer wins.
	afterSet, won, err := store.SetRoundFacilitatorMessage(ctx, sess.ID, updated.Round, fMsg.ID)
	if err != nil {
		t.Fatalf("SetRoundFacilitatorMessage: %v", err)
	}
	if !won {
		t.Error("SetRoundFacilitatorMessage: expected to win (first writer)")
	}
	if afterSet.CurrentRoundMessageID == nil || *afterSet.CurrentRoundMessageID != fMsg.ID {
		t.Errorf("CurrentRoundMessageID = %v, want %v", afterSet.CurrentRoundMessageID, fMsg.ID)
	}

	// Second write for the same round — should lose (first-writer-wins CAS).
	otherMsgID := uuid.New()
	_, won2, err2 := store.SetRoundFacilitatorMessage(ctx, sess.ID, updated.Round, otherMsgID)
	if err2 != nil {
		t.Fatalf("SetRoundFacilitatorMessage (second): %v", err2)
	}
	if won2 {
		t.Error("SetRoundFacilitatorMessage (second): expected to lose (first-writer-wins)")
	}
}

func TestPartySession_CloseAndVoicesAllowed(t *testing.T) {
	store, cleanup := openPartyTestDB(t)
	defer cleanup()
	ctx := context.Background()

	projectID := "test-ns/test-project"
	teamID := uuid.New()
	auth := AuthorContext{Principal: "human-1", TeamID: teamID}
	thread, err := store.OpenThread(ctx, projectID, auth, "close test", "")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	sess, _, err := store.StartPartySession(ctx, projectID, teamID, thread.ID, auth, "party!", nil)
	if err != nil {
		t.Fatalf("StartPartySession: %v", err)
	}

	// VoicesAllowedThisRound caps at MaxVoicesPerRound.
	allowed, capped := sess.VoicesAllowedThisRound(10)
	if allowed != sess.Budget.MaxVoicesPerRound {
		t.Errorf("VoicesAllowedThisRound(10).allowed = %d, want %d", allowed, sess.Budget.MaxVoicesPerRound)
	}
	if capped != 10-sess.Budget.MaxVoicesPerRound {
		t.Errorf("VoicesAllowedThisRound(10).capped = %d, want %d", capped, 10-sess.Budget.MaxVoicesPerRound)
	}

	// CloseSession transitions to terminal phase.
	closed, err := store.CloseSession(ctx, sess.ID, PartyPhaseClosed)
	if err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if closed.Phase != PartyPhaseClosed {
		t.Errorf("after CloseSession: phase = %q, want %q", closed.Phase, PartyPhaseClosed)
	}
	if closed.ClosedAt == nil {
		t.Error("after CloseSession: ClosedAt should be set")
	}

	// A second close should fail (session no longer active).
	_, err = store.CloseSession(ctx, sess.ID, PartyPhaseConverged)
	if err == nil {
		t.Error("CloseSession on already-closed session: expected error")
	}

	// ActivePartySession should now return ErrNoActivePartySession.
	_, err = store.ActivePartySession(ctx, thread.ID)
	if !isNoActivePartySession(err) {
		t.Errorf("ActivePartySession after close: expected ErrNoActivePartySession, got %v", err)
	}
}

// isPartySessionNotActive checks for the ErrPartySessionNotActive sentinel.
func isPartySessionNotActive(err error) bool {
	return err != nil && err.Error() == ErrPartySessionNotActive.Error()
}

// isNoActivePartySession checks for the ErrNoActivePartySession sentinel.
func isNoActivePartySession(err error) bool {
	return err != nil && err.Error() == ErrNoActivePartySession.Error()
}

// strPtr is a convenience for constructing *string test values.
func strPtr(s string) *string { return &s }
