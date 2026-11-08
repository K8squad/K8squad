//go:build decision_integration

// ISI-7224 (ISI-7074 WS-6.2) — the acceptance chain of the decision-raise
// emission path, against real Postgres, exactly like the discussion
// integration lane: DATABASE_URL unset ⇒ SKIP.
//
//	agent raises (Raiser over the REAL dispatch ledger + REAL store)
//	  → the open card is visible to the inbox arm (ListOpenDecisionRequestsForTeam)
//	  → a retry of the same ask returns the SAME card (deterministic idempotency)
//	  → the human answer resolves the card (AnswerDecisionRequest) — the
//	    resume_agent_on_answer re-dispatch leg this relies on is covered by the
//	    apiserver answer-shell tests (internal/apiserver, request-confirmation
//	    fan-out through WorkItemDispatcher.RequestDispatch); nothing here adds
//	    custody semantics.
package decisionraise

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/mentiondispatch"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping the decision-raise integration test (needs real Postgres)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return db
}

// applyMigrations walks the SHIPPED migrations the raise chain touches: the
// coord base (work_item + claim), the discussion thread/message chain, the
// proposal kind CHECK (0025) the decision table widens (0031), and the mention
// dispatch ledger (0027) the thread resolver reads.
func applyMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS discussion CASCADE; DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset schemas: %v", err)
	}
	for _, name := range []string{
		"0001_coord_schema.sql",
		"0004_discussion_schema.sql",
		"0024_discussion_message_fields.sql",
		"0026_discussion_project_id_text.sql",
		"0025_discussion_proposal.sql",
		"0027_dispatch_on_mention_run_minting.sql",
		"0031_discussion_decision_request.sql",
	} {
		var sqlBytes []byte
		var err error
		for _, c := range []string{
			filepath.Join("..", "..", "db", "migrations", name),
			filepath.Join("db", "migrations", name),
		} {
			if sqlBytes, err = os.ReadFile(c); err == nil {
				break
			}
		}
		if sqlBytes == nil {
			t.Fatalf("could not locate %s", name)
		}
		if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("migration %s failed: %v", name, err)
		}
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = db.ExecContext(cctx, `DROP SCHEMA IF EXISTS discussion CASCADE; DROP SCHEMA IF EXISTS coord CASCADE`)
	})
}

// seedLedger builds the room-thread dispatch a raising run is bound to:
// thread + triggering message + drive work item + claim(run) + mention_dispatch.
func seedLedger(t *testing.T, db *sql.DB, runID uuid.UUID) (projectID string, teamID, threadID, workItemID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	projectID = "ns/raise-it"
	teamID, threadID, workItemID = uuid.New(), uuid.New(), uuid.New()
	triggerMsg := uuid.New()
	stmts := []string{
		`INSERT INTO discussion.thread (id, project_id, team_id, title, created_by) VALUES ($1, $2, $3, 'Raise room', 'board-human')`,
		`INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind) VALUES ($1, $2, 'board-human', '@alfred decide this', 'party', 'text')`,
		`INSERT INTO coord.work_item (id, project_id, team_id, title, state, created_by) VALUES ($1, $2, $3, 'Thread drive item', 'in_progress', 'ksquad-mention')`,
		`INSERT INTO coord.claim (work_item_id, holder_principal, run_id, fence_token) VALUES ($1, 'alfred', $2, 1)`,
		`INSERT INTO discussion.mention_dispatch (message_id, agent_name, project_id, thread_id, hop_depth, work_item_id) VALUES ($1, 'alfred', $2, $3, 1, $4)`,
	}
	args := [][]any{
		{threadID, projectID, teamID},
		{triggerMsg, threadID},
		{workItemID, uuid.New(), teamID},
		{workItemID, runID},
		{triggerMsg, projectID, threadID, workItemID},
	}
	for i, q := range stmts {
		if _, err := db.ExecContext(ctx, q, args[i]...); err != nil {
			t.Fatalf("seed stmt %d: %v", i, err)
		}
	}
	return projectID, teamID, threadID, workItemID
}

func TestRaiseShowsUpInInboxArmAndAnswers(t *testing.T) {
	db := openDB(t)
	applyMigrations(t, db)
	runID := uuid.New()
	projectID, teamID, _, workItemID := seedLedger(t, db, runID)

	store := discussion.NewStore(db)
	raiser, err := New(mentiondispatch.NewProposalThreadResolver(db), store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ask := discussion.DecisionRaise{
		Title:       "Which rollout window?",
		Details:     "pick one",
		Options:     []string{"canary 10%", "full", "hold"},
		Recommended: 1,
	}
	msg, err := raiser.RequestDecision(context.Background(), runID.String(), "alfred", workItemID.String(), ask)
	if err != nil {
		t.Fatalf("raise: %v", err)
	}

	// The inbox arm (GET /api/squad/inbox decision union member) must carry the
	// open card, bound to the raising ticket.
	open, err := store.ListOpenDecisionRequestsForTeam(context.Background(), teamID.String())
	if err != nil {
		t.Fatalf("inbox arm: %v", err)
	}
	var found bool
	for _, o := range open {
		if o.MessageID == msg.ID.String() {
			found = true
			if o.Title != ask.Title || o.Mode != discussion.DecisionModeChooseOne {
				t.Fatalf("card summary = %+v", o)
			}
			if o.TicketID != workItemID.String() {
				t.Fatalf("card ticket = %q, want the raising work item", o.TicketID)
			}
		}
	}
	if !found {
		t.Fatalf("raised card %s not in the inbox arm: %+v", msg.ID, open)
	}

	// A retry of the same ask is idempotent: same card, no duplicate question.
	again, err := raiser.RequestDecision(context.Background(), runID.String(), "alfred", workItemID.String(), ask)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.ID != msg.ID {
		t.Fatalf("retry posted a second card: %s vs %s", again.ID, msg.ID)
	}

	// The human answer resolves the card (the answer shell + re-dispatch leg is
	// the existing apiserver surface; the store CAS is the shared bottom).
	human := discussion.AuthorContext{Principal: "board-human", TeamID: teamID}
	answered, err := store.AnswerDecisionRequest(context.Background(), projectID, teamID, msg.ID, human, discussion.DecisionAnswer{
		Mode:              discussion.DecisionModeChooseOne,
		SelectedOptionIDs: []string{"a"},
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	card, err := store.GetDecisionRequest(context.Background(), projectID, teamID, msg.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if card.Phase != "answered" {
		t.Fatalf("phase = %q, want answered", card.Phase)
	}
	if answered == nil || card.Answer == nil || len(card.Answer.SelectedOptionIDs) != 1 || card.Answer.SelectedOptionIDs[0] != "a" {
		t.Fatalf("answer did not land: %+v", card.Answer)
	}
	// Answered cards leave the open inbox arm.
	open2, err := store.ListOpenDecisionRequestsForTeam(context.Background(), teamID.String())
	if err != nil {
		t.Fatalf("inbox arm after answer: %v", err)
	}
	for _, o := range open2 {
		if o.MessageID == msg.ID.String() {
			t.Fatalf("answered card still open in the inbox arm")
		}
	}

	// And a raise from a run with NO ledger row still refuses honestly.
	_, err = raiser.RequestDecision(context.Background(), uuid.New().String(), "alfred", workItemID.String(), ask)
	if !errors.Is(err, ErrNoRaisingThread) {
		t.Fatalf("threadless run: err = %v, want ErrNoRaisingThread", err)
	}
}
