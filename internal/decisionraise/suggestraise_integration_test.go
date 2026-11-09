//go:build decision_integration

// ISI-7218 (ISI-7075 C4) — the acceptance chain of the suggest-next-steps
// emission path, against real Postgres, exactly like the decision-raise lane it
// extends: DATABASE_URL unset ⇒ SKIP.
//
//	coordinator reports blocked (SuggestNextSteps over the REAL dispatch ledger
//	+ REAL store, migrations incl. the 0042 kind widening)
//	  → the open card is kind='suggest_next_steps' and rides the SHARED lifecycle
//	    (GetDecisionRequest sees it; phase=open; options auto-lettered)
//	  → the inbox arm (ListOpenDecisionRequestsForTeam) carries it, bound to the
//	    raising ticket
//	  → a retry of the same ask returns the SAME card (deterministic idempotency)
//	  → "blocked with zero suggested next steps" is refused (taxonomy §5 rule 1)
//	  → the human answer resolves the card through the EXISTING answer CAS —
//	    no new state machine anywhere.
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

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/mentiondispatch"
)

// applySuggestMigrations layers the 0042 kind widening on top of the raise
// chain's shipped migrations (see applyMigrations above).
func applySuggestMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	applyMigrations(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := "0042_discussion_suggest_next_steps.sql"
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

func TestSuggestNextStepsLifecycle(t *testing.T) {
	db := openDB(t)
	applySuggestMigrations(t, db)
	runID := uuid.New()
	projectID, teamID, _, workItemID := seedLedger(t, db, runID)

	store := discussion.NewStore(db)
	raiser, err := New(mentiondispatch.NewProposalThreadResolver(db), store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	raiser = raiser.WithSuggestions(store)

	ask := discussion.SuggestNextStepsRaise{
		Title:   "Blocked: mesh endpoint down — next steps?",
		Details: "squad parked on the integration lane",
		Steps: []discussion.SuggestedStep{
			{Label: "Create child for endpoint failover", Description: "implementer owns it; unblocks the lane", Recommended: true},
			{Label: "Reassign the conformance run to the reviewer", Description: "keeps the lane warm while failover lands"},
		},
	}
	msg, err := raiser.SuggestNextSteps(context.Background(), runID.String(), "alfred", workItemID.String(), ask)
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}

	// The card is kind='suggest_next_steps' and rides the SHARED lifecycle: the
	// decision read returns it with phase=open and the auto-lettered options.
	card, err := store.GetDecisionRequest(context.Background(), projectID, teamID, msg.ID)
	if err != nil {
		t.Fatalf("GetDecisionRequest: %v", err)
	}
	if card.Message.Kind != discussion.KindSuggestNextSteps {
		t.Fatalf("kind = %q, want %q", card.Message.Kind, discussion.KindSuggestNextSteps)
	}
	if card.Phase != discussion.DecisionPhaseOpen || card.Continuation != discussion.DefaultContinuation {
		t.Fatalf("phase/continuation = %q/%q", card.Phase, card.Continuation)
	}
	if len(card.Payload.Options) != 2 || card.Payload.Options[0].ID != "a" || !card.Payload.Options[0].Recommended {
		t.Fatalf("options = %+v", card.Payload.Options)
	}
	if card.WorkItemID != workItemID.String() {
		t.Fatalf("workItemID = %q, want the raising work item %q", card.WorkItemID, workItemID)
	}

	// The inbox arm carries the open suggest card, bound to the raising ticket.
	open, err := store.ListOpenDecisionRequestsForTeam(context.Background(), teamID.String())
	if err != nil {
		t.Fatalf("inbox arm: %v", err)
	}
	var found bool
	for _, o := range open {
		if o.MessageID == msg.ID.String() {
			found = true
			if o.Title != ask.Title || o.TicketID != workItemID.String() {
				t.Fatalf("card summary = %+v", o)
			}
		}
	}
	if !found {
		t.Fatalf("suggest card %s not in the inbox arm: %+v", msg.ID, open)
	}

	// Idempotent retry: same ask ⇒ same card, no duplicate question.
	again, err := raiser.SuggestNextSteps(context.Background(), runID.String(), "alfred", workItemID.String(), ask)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.ID != msg.ID {
		t.Fatalf("retry posted a second card: %s vs %s", again.ID, msg.ID)
	}

	// Taxonomy §5 rule 1: blocked with zero suggested next steps is refused.
	if _, err := raiser.SuggestNextSteps(context.Background(), runID.String(), "alfred", workItemID.String(), discussion.SuggestNextStepsRaise{Title: "Blocked"}); !errors.Is(err, discussion.ErrInvalidDecisionPayload) {
		t.Fatalf("zero-step error = %v, want ErrInvalidDecisionPayload", err)
	}

	// The human answer resolves the card through the EXISTING CAS — one state
	// machine for both kinds.
	human := discussion.AuthorContext{Principal: "board-human", TeamID: teamID}
	if _, err := store.AnswerDecisionRequest(context.Background(), projectID, teamID, msg.ID, human, discussion.DecisionAnswer{
		Mode:              discussion.DecisionModeChooseOne,
		SelectedOptionIDs: []string{"a"},
	}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	answered, err := store.GetDecisionRequest(context.Background(), projectID, teamID, msg.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if answered.Phase != discussion.DecisionPhaseAnswered || answered.Answer == nil || len(answered.Answer.SelectedOptionIDs) != 1 || answered.Answer.SelectedOptionIDs[0] != "a" {
		t.Fatalf("answered card = phase %q answer %+v", answered.Phase, answered.Answer)
	}
}
