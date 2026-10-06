//go:build discussion_integration

// ISI-5536 (ISI-5531 E2) — decision_request lifecycle integration tests against real Postgres. Build-
// tag gated exactly like proposal_integration_test.go; CI provisions Postgres and runs
//
//	go test -tags=discussion_integration ./internal/discussion/...
//
// It applies the SHIPPED migrations (0004 → 0024 → 0026 → 0025 → 0031), not inline DDL, so drift
// between the migrations and the code goes RED here. DATABASE_URL unset ⇒ SKIP (openTestDB).
//
// This is the DB-backed coverage the unit tests cannot provide (decision_test.go is store-free): the
// idempotent create, the tenant-scoped idempotency (M5), the answer/reject/expire CAS single-winner
// (M4), the revision-moved supersede sweep (M3), and the cross-team tenancy fence.
package discussion

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// applyDecisionMigrations layers the proposal CHECK (0025) and the decision side table (0031) on top
// of the shared discussion chain (0004 + 0024 + 0026) that applyMigration installs. 0031 widens the
// kind CHECK 0025 defined, so 0025 MUST precede it.
func applyDecisionMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	applyMigration(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{"0025_discussion_proposal.sql", "0031_discussion_decision_request.sql"} {
		candidates := []string{
			filepath.Join("..", "..", "db", "migrations", name),
			filepath.Join("db", "migrations", name),
		}
		if d := os.Getenv("DISCUSSION_MIGRATIONS_DIR"); d != "" {
			candidates = append([]string{filepath.Join(d, name)}, candidates...)
		}
		var sqlBytes []byte
		var err error
		for _, c := range candidates {
			if sqlBytes, err = os.ReadFile(c); err == nil {
				break
			}
		}
		if sqlBytes == nil {
			t.Fatalf("could not locate %s (tried %v)", name, candidates)
		}
		if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("migration %s failed against real Postgres: %v", name, err)
		}
	}
}

func decisionFixtures(t *testing.T) (store *Store, projectID string, teamID, threadID uuid.UUID, agent, human AuthorContext) {
	t.Helper()
	db := openTestDB(t)
	applyDecisionMigrations(t, db)
	store = NewStore(db)

	projectID, teamID = "test-ns/test-project", uuid.New()
	agentID, runID := "agent:winston", "run:r1"
	agent = AuthorContext{Principal: agentID, TeamID: teamID, AgentID: &agentID, RunID: &runID}
	human = AuthorContext{Principal: "user:reviewer", TeamID: teamID}

	th, err := store.OpenThread(context.Background(), projectID, human, "Decide the client", "Which HTTP client?")
	if err != nil {
		t.Fatalf("open thread: %v", err)
	}
	threadID = th.ID
	return
}

func chooseOnePayload() DecisionRequestPayload {
	return DecisionRequestPayload{
		Version: 1,
		Mode:    DecisionModeChooseOne,
		Title:   "Which HTTP client?",
		Options: []DecisionOption{{ID: "reqwest", Label: "reqwest", Recommended: true}, {ID: "hyper", Label: "hyper"}},
	}
}

// TestDecisionAnswerCASSingleWinner — the core CAS the reviewer flagged as untested (M4): an inert
// agent ask, a human answer open→answered with a threaded post-back, and the one-shot guarantee that a
// second answer (and a reject) on the decided card is refused with ErrDecisionRequestNotOpen.
func TestDecisionAnswerCASSingleWinner(t *testing.T) {
	store, projectID, teamID, threadID, agent, human := decisionFixtures(t)
	ctx := context.Background()

	msg, err := store.PostDecisionRequest(ctx, projectID, teamID, threadID, agent,
		"Which HTTP client should we use?", chooseOnePayload(), nil, "", "decision:wi-1:http:r1", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if msg.Kind != KindDecisionRequest {
		t.Fatalf("kind: got %q want decision_request", msg.Kind)
	}
	dr, err := store.GetDecisionRequest(ctx, projectID, teamID, msg.ID)
	if err != nil || dr.Phase != DecisionPhaseOpen {
		t.Fatalf("get after post: %+v %v", dr, err)
	}
	if dr.Message.AuthorAgentID == nil || *dr.Message.AuthorAgentID != "agent:winston" {
		t.Fatalf("proposer provenance not server-stamped: %+v", dr.Message)
	}

	answer := DecisionAnswer{Mode: DecisionModeChooseOne, SelectedOptionIDs: []string{"reqwest"}}
	postBack, err := store.AnswerDecisionRequest(ctx, projectID, teamID, msg.ID, human, answer)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if postBack.ParentID == nil || *postBack.ParentID != msg.ID || postBack.Kind != KindStructured {
		t.Fatalf("post-back must be a structured reply on the card: %+v", postBack)
	}
	dr, _ = store.GetDecisionRequest(ctx, projectID, teamID, msg.ID)
	if dr.Phase != DecisionPhaseAnswered || dr.AnsweredBy != human.Principal || dr.Answer == nil {
		t.Fatalf("after answer: %+v", dr)
	}
	if len(dr.Answer.SelectedOptionIDs) != 1 || dr.Answer.SelectedOptionIDs[0] != "reqwest" {
		t.Fatalf("answer selection not persisted: %+v", dr.Answer)
	}

	// One-shot: the card already left `open`, so a second answer and a reject are both refused.
	if _, err := store.AnswerDecisionRequest(ctx, projectID, teamID, msg.ID, human, answer); !errors.Is(err, ErrDecisionRequestNotOpen) {
		t.Fatalf("double answer: want ErrDecisionRequestNotOpen, got %v", err)
	}
	if _, err := store.RejectDecisionRequest(ctx, projectID, teamID, msg.ID, human, "too late"); !errors.Is(err, ErrDecisionRequestNotOpen) {
		t.Fatalf("reject after answer: want ErrDecisionRequestNotOpen, got %v", err)
	}
}

// TestDecisionReject — a human rejection records open→rejected with the reason and a threaded
// post-back; a second reject is refused.
func TestDecisionReject(t *testing.T) {
	store, projectID, teamID, threadID, agent, human := decisionFixtures(t)
	ctx := context.Background()

	msg, err := store.PostDecisionRequest(ctx, projectID, teamID, threadID, agent,
		"Which HTTP client?", chooseOnePayload(), nil, "", "decision:wi-1:http:r2", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	postBack, err := store.RejectDecisionRequest(ctx, projectID, teamID, msg.ID, human, "neither fits")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if postBack.ParentID == nil || *postBack.ParentID != msg.ID {
		t.Fatalf("reject post-back not threaded under the card: %+v", postBack)
	}
	dr, _ := store.GetDecisionRequest(ctx, projectID, teamID, msg.ID)
	if dr.Phase != DecisionPhaseRejected || dr.RejectReason != "neither fits" || dr.Answer == nil || !dr.Answer.Rejected {
		t.Fatalf("after reject: %+v", dr)
	}
	if _, err := store.RejectDecisionRequest(ctx, projectID, teamID, msg.ID, human, "again"); !errors.Is(err, ErrDecisionRequestNotOpen) {
		t.Fatalf("double reject: want ErrDecisionRequestNotOpen, got %v", err)
	}
}

// TestDecisionIdempotentCreate — a retry re-posting the SAME (team, idempotencyKey) returns the
// EXISTING card (same message id), never a second row (ADR-0026 §4.2).
func TestDecisionIdempotentCreate(t *testing.T) {
	store, projectID, teamID, threadID, agent, _ := decisionFixtures(t)
	ctx := context.Background()

	key := "decision:wi-7:http:r1"
	first, err := store.PostDecisionRequest(ctx, projectID, teamID, threadID, agent, "ask", chooseOnePayload(), nil, "", key, nil)
	if err != nil {
		t.Fatalf("first post: %v", err)
	}
	second, err := store.PostDecisionRequest(ctx, projectID, teamID, threadID, agent, "ask again", chooseOnePayload(), nil, "", key, nil)
	if err != nil {
		t.Fatalf("retry post: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotent create minted a second card: %s vs %s", first.ID, second.ID)
	}
	list, err := store.ListDecisionRequests(ctx, projectID, teamID, threadID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want exactly one card for a re-used key, got %d", len(list))
	}
}

// TestDecisionIdempotencyIsTenantScoped — the M5 fix: the SAME idempotencyKey in two DIFFERENT teams
// must BOTH create a card. A global-unique key would let team A's row block team B's create (the
// insert conflicts, but team B's tenancy-scoped re-read cannot see team A's row → a spurious error).
func TestDecisionIdempotencyIsTenantScoped(t *testing.T) {
	store, projectID, teamA, threadA, agentA, _ := decisionFixtures(t)
	ctx := context.Background()

	// A second team with its own thread in the same project.
	teamB := uuid.New()
	humanB := AuthorContext{Principal: "user:b", TeamID: teamB}
	thB, err := store.OpenThread(ctx, projectID, humanB, "Team B room", "B decides too")
	if err != nil {
		t.Fatalf("open thread B: %v", err)
	}
	agentBID := "agent:b"
	agentB := AuthorContext{Principal: agentBID, TeamID: teamB, AgentID: &agentBID}

	key := "decision:shared:http:r1" // the very same key in both tenants
	a, err := store.PostDecisionRequest(ctx, projectID, teamA, threadA, agentA, "A asks", chooseOnePayload(), nil, "", key, nil)
	if err != nil {
		t.Fatalf("team A post: %v", err)
	}
	b, err := store.PostDecisionRequest(ctx, projectID, teamB, thB.ID, agentB, "B asks", chooseOnePayload(), nil, "", key, nil)
	if err != nil {
		t.Fatalf("team B post (M5 regression — same key cross-tenant must not block): %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("cross-tenant same key collapsed to one card: %s", a.ID)
	}
	// Each team sees exactly its own card.
	if dr, err := store.GetDecisionRequest(ctx, projectID, teamA, a.ID); err != nil || dr.Message.ID != a.ID {
		t.Fatalf("team A cannot read its own card: %+v %v", dr, err)
	}
	if dr, err := store.GetDecisionRequest(ctx, projectID, teamB, b.ID); err != nil || dr.Message.ID != b.ID {
		t.Fatalf("team B cannot read its own card: %+v %v", dr, err)
	}
}

// TestDecisionSupersedeOnNewerRevision — the M3 anti-nag sweep: posting a fresh card bound to the SAME
// target ref but a NEWER revision auto-supersedes the prior open card on that target; a card on a
// DIFFERENT ref is untouched, and the new card stays open.
func TestDecisionSupersedeOnNewerRevision(t *testing.T) {
	store, projectID, teamID, threadID, agent, _ := decisionFixtures(t)
	ctx := context.Background()

	targetV1 := &DecisionTarget{Type: "plan", Ref: "plan-1", RevisionID: "v1"}
	stale, err := store.PostDecisionRequest(ctx, projectID, teamID, threadID, agent, "ask v1", chooseOnePayload(), targetV1, "", "decision:plan-1:v1", nil)
	if err != nil {
		t.Fatalf("post v1: %v", err)
	}
	// An unrelated card on a different target — must NOT be swept.
	otherTarget := &DecisionTarget{Type: "plan", Ref: "plan-2", RevisionID: "v1"}
	other, err := store.PostDecisionRequest(ctx, projectID, teamID, threadID, agent, "unrelated", chooseOnePayload(), otherTarget, "", "decision:plan-2:v1", nil)
	if err != nil {
		t.Fatalf("post other: %v", err)
	}

	// The revision moves: a new card on plan-1 @ v2. PostDecisionRequest sweeps plan-1 @ v1.
	targetV2 := &DecisionTarget{Type: "plan", Ref: "plan-1", RevisionID: "v2"}
	fresh, err := store.PostDecisionRequest(ctx, projectID, teamID, threadID, agent, "ask v2", chooseOnePayload(), targetV2, "", "decision:plan-1:v2", nil)
	if err != nil {
		t.Fatalf("post v2: %v", err)
	}

	if dr, _ := store.GetDecisionRequest(ctx, projectID, teamID, stale.ID); dr.Phase != DecisionPhaseSuperseded {
		t.Fatalf("stale v1 card should be superseded, got %q", dr.Phase)
	}
	if dr, _ := store.GetDecisionRequest(ctx, projectID, teamID, fresh.ID); dr.Phase != DecisionPhaseOpen {
		t.Fatalf("fresh v2 card should stay open, got %q", dr.Phase)
	}
	if dr, _ := store.GetDecisionRequest(ctx, projectID, teamID, other.ID); dr.Phase != DecisionPhaseOpen {
		t.Fatalf("unrelated-target card must not be swept, got %q", dr.Phase)
	}
}

// TestExpireDecisionRequest — the single-card expire primitive (the sweep's building block): it
// CAS-advances open→expired, rejects a bad target phase, and refuses a card that already left open.
func TestExpireDecisionRequest(t *testing.T) {
	store, projectID, teamID, threadID, agent, _ := decisionFixtures(t)
	ctx := context.Background()

	msg, err := store.PostDecisionRequest(ctx, projectID, teamID, threadID, agent, "ask", chooseOnePayload(), nil, "", "decision:exp:1", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := store.ExpireDecisionRequest(ctx, projectID, teamID, msg.ID, "nonsense"); !errors.Is(err, ErrInvalidDecisionPayload) {
		t.Fatalf("bad expire target: want ErrInvalidDecisionPayload, got %v", err)
	}
	if err := store.ExpireDecisionRequest(ctx, projectID, teamID, msg.ID, DecisionPhaseExpired); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if dr, _ := store.GetDecisionRequest(ctx, projectID, teamID, msg.ID); dr.Phase != DecisionPhaseExpired {
		t.Fatalf("after expire: %+v", dr)
	}
	if err := store.ExpireDecisionRequest(ctx, projectID, teamID, msg.ID, DecisionPhaseExpired); !errors.Is(err, ErrDecisionRequestNotOpen) {
		t.Fatalf("double expire: want ErrDecisionRequestNotOpen, got %v", err)
	}
}

// TestDecisionTenancyFence — a decision_request in another team's room is invisible (404-not-403):
// get, answer, and reject all hide existence cross-tenant, while the owner's view is untouched.
func TestDecisionTenancyFence(t *testing.T) {
	store, projectID, teamID, threadID, agent, _ := decisionFixtures(t)
	ctx := context.Background()

	msg, err := store.PostDecisionRequest(ctx, projectID, teamID, threadID, agent, "private ask", chooseOnePayload(), nil, "", "decision:fence:1", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	otherTeam := uuid.New()
	outsider := AuthorContext{Principal: "user:eve", TeamID: otherTeam}
	if _, err := store.GetDecisionRequest(ctx, projectID, otherTeam, msg.ID); !errors.Is(err, ErrDecisionRequestNotFound) {
		t.Fatalf("cross-team get: want ErrDecisionRequestNotFound, got %v", err)
	}
	if _, err := store.AnswerDecisionRequest(ctx, projectID, otherTeam, msg.ID, outsider, DecisionAnswer{Mode: DecisionModeChooseOne, SelectedOptionIDs: []string{"reqwest"}}); !errors.Is(err, ErrDecisionRequestNotFound) {
		t.Fatalf("cross-team answer: want ErrDecisionRequestNotFound, got %v", err)
	}
	if _, err := store.RejectDecisionRequest(ctx, projectID, otherTeam, msg.ID, outsider, "no"); !errors.Is(err, ErrDecisionRequestNotFound) {
		t.Fatalf("cross-team reject: want ErrDecisionRequestNotFound, got %v", err)
	}
	// The owner's card is untouched by the outsider probes.
	if dr, err := store.GetDecisionRequest(ctx, projectID, teamID, msg.ID); err != nil || dr.Phase != DecisionPhaseOpen {
		t.Fatalf("owner view after outsider probes: %+v %v", dr, err)
	}
}

// TestDecisionKindCheckConstraint — the widened kind CHECK (0031) admits 'decision_request' on insert
// and the created_at is server-stamped.
func TestDecisionKindCheckConstraint(t *testing.T) {
	store, projectID, teamID, threadID, agent, _ := decisionFixtures(t)
	msg, err := store.PostDecisionRequest(context.Background(), projectID, teamID, threadID, agent, "kind check",
		DecisionRequestPayload{Version: 1, Mode: DecisionModeApprove, Title: "ship?"}, nil, "", "decision:kind:1", nil)
	if err != nil {
		t.Fatalf("decision_request kind rejected by the CHECK constraint: %v", err)
	}
	if msg.CreatedAt.IsZero() || time.Since(msg.CreatedAt) > time.Minute {
		t.Fatalf("created_at not server-stamped: %+v", msg)
	}
}
