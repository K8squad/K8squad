//go:build discussion_integration

// ISI-4928 — proposal lifecycle integration tests against real Postgres. Build-tag gated like the
// room's other integration lanes; CI provisions Postgres and runs
//
//	go test -tags=discussion_integration ./internal/discussion/...
//
// It applies the SHIPPED migrations (0004 → 0024 → 0025), not inline DDL, so drift between the
// migrations and the code goes RED here. DATABASE_URL unset ⇒ SKIP (same posture as integration_test).
package discussion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func applyProposalMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	applyMigration(t, db) // 0004: schema reset + thread/message + append-only triggers
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{"0024_discussion_message_fields.sql", "0025_discussion_proposal.sql"} {
		var sqlBytes []byte
		var err error
		candidates := []string{
			filepath.Join("..", "..", "db", "migrations", name),
			filepath.Join("db", "migrations", name),
		}
		if d := os.Getenv("DISCUSSION_MIGRATIONS_DIR"); d != "" {
			candidates = append([]string{filepath.Join(d, name)}, candidates...)
		}
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

func proposalFixtures(t *testing.T) (store *Store, projectID, teamID uuid.UUID, threadID uuid.UUID, agent, human AuthorContext) {
	t.Helper()
	db := openTestDB(t)
	applyProposalMigrations(t, db)
	store = NewStore(db)

	projectID, teamID = uuid.New(), uuid.New()
	agentID, runID := "agent:coordinator", "run:r1"
	agent = AuthorContext{Principal: agentID, TeamID: teamID, AgentID: &agentID, RunID: &runID}
	human = AuthorContext{Principal: "user:reviewer", TeamID: teamID} // ANY human in the room (OQ4)

	th, err := store.OpenThread(context.Background(), projectID, human, "Deploy review", "Where should this land?")
	if err != nil {
		t.Fatalf("open thread: %v", err)
	}
	threadID = th.ID
	return
}

// TestProposalRoundTripAndLifecycle — the full decision machine against real Postgres: an inert
// agent proposal, a confirm by a DIFFERENT human (any-human, OQ4), the executed post-back, and the
// one-shot CAS on every transition.
func TestProposalRoundTripAndLifecycle(t *testing.T) {
	store, projectID, teamID, threadID, agent, human := proposalFixtures(t)
	ctx := context.Background()

	// 1. Agent proposes — inert: message + lifecycle row, nothing else.
	msg, err := store.PostProposal(ctx, projectID, teamID, threadID, agent,
		"Propose we file this as a ticket", ProposalPayload{Action: ProposalActionCreateTicket, Title: "Ship the room", Body: "from the room"}, nil)
	if err != nil {
		t.Fatalf("post proposal: %v", err)
	}
	if msg.Kind != KindProposal {
		t.Fatalf("kind: got %q want proposal", msg.Kind)
	}
	p, err := store.GetProposal(ctx, projectID, teamID, msg.ID)
	if err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	if p.Phase != ProposalPhaseProposed || p.Payload.Action != ProposalActionCreateTicket || p.Payload.Title != "Ship the room" {
		t.Fatalf("proposal: %+v", p)
	}
	if p.Message.AuthorAgentID == nil || *p.Message.AuthorAgentID != "agent:coordinator" {
		t.Fatalf("proposer provenance not server-stamped: %+v", p.Message)
	}

	// 2. A different human confirms — any human in the room, not just the author.
	if _, err := store.ConfirmProposal(ctx, projectID, teamID, msg.ID, human); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if p, _ = store.GetProposal(ctx, projectID, teamID, msg.ID); p.Phase != ProposalPhaseConfirmed || p.DecidedBy != human.Principal {
		t.Fatalf("after confirm: %+v", p)
	}

	// 3. One-shot CAS: a second confirm (or a dismiss) on a decided card is refused.
	if _, err := store.ConfirmProposal(ctx, projectID, teamID, msg.ID, human); !errors.Is(err, ErrProposalNotProposed) {
		t.Fatalf("double confirm: want ErrProposalNotProposed, got %v", err)
	}
	if err := store.DismissProposal(ctx, projectID, teamID, msg.ID, human); !errors.Is(err, ErrProposalNotProposed) {
		t.Fatalf("dismiss after confirm: want ErrProposalNotProposed, got %v", err)
	}

	// 4. Execute + post-back: the reply lands under the card as kind=structured, and the phase
	//    (and only then) is executed.
	result, _ := json.Marshal(map[string]any{"action": "create_ticket", "workItemId": "wi-9"})
	postBack, err := store.CompleteProposal(ctx, projectID, teamID, msg.ID, human, result, "Proposal confirmed: created work item wi-9")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if postBack.ParentID == nil || *postBack.ParentID != msg.ID || postBack.Kind != KindStructured {
		t.Fatalf("post-back must be a structured reply on the card: %+v", postBack)
	}
	if p, _ = store.GetProposal(ctx, projectID, teamID, msg.ID); p.Phase != ProposalPhaseExecuted {
		t.Fatalf("after complete: %+v", p)
	}
	th, err := store.GetThread(ctx, projectID, teamID, threadID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	var card *Message
	for i := range th.Messages {
		if th.Messages[i].ID == msg.ID {
			card = &th.Messages[i]
		}
	}
	if len(th.Messages) != 2 || card == nil || len(card.Replies) != 1 || card.Replies[0].ID != postBack.ID {
		t.Fatalf("post-back not threaded under the proposal: %+v", th.Messages)
	}
}

// TestProposalDismissIsDecisionOnly — dismiss records proposed→dismissed and never touches
// anything else; the card stays visible with its decided_by stamp.
func TestProposalDismissIsDecisionOnly(t *testing.T) {
	store, projectID, teamID, threadID, agent, human := proposalFixtures(t)
	ctx := context.Background()

	msg, err := store.PostProposal(ctx, projectID, teamID, threadID, agent, "Bad idea?",
		ProposalPayload{Action: ProposalActionPartyRun, Title: "party: rewrite in cobol"}, nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := store.DismissProposal(ctx, projectID, teamID, msg.ID, human); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	p, err := store.GetProposal(ctx, projectID, teamID, msg.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.Phase != ProposalPhaseDismissed || p.DecidedBy != human.Principal || p.DecidedAt == nil {
		t.Fatalf("after dismiss: %+v", p)
	}
	if err := store.DismissProposal(ctx, projectID, teamID, msg.ID, human); !errors.Is(err, ErrProposalNotProposed) {
		t.Fatalf("double dismiss: want ErrProposalNotProposed, got %v", err)
	}
}

// TestProposalFailRollsBackForRetry — a failed fan-out returns the card to proposed so the human
// can retry or dismiss; the lifecycle never claims an execution that did not happen.
func TestProposalFailRollsBackForRetry(t *testing.T) {
	store, projectID, teamID, threadID, agent, human := proposalFixtures(t)
	ctx := context.Background()

	msg, err := store.PostProposal(ctx, projectID, teamID, threadID, agent, "Retry me",
		ProposalPayload{Action: ProposalActionAssignAgent, TicketID: "wi-1", AssigneeAgentID: "a:kimi"}, nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := store.ConfirmProposal(ctx, projectID, teamID, msg.ID, human); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := store.FailProposal(ctx, projectID, teamID, msg.ID); err != nil {
		t.Fatalf("fail: %v", err)
	}
	p, _ := store.GetProposal(ctx, projectID, teamID, msg.ID)
	if p.Phase != ProposalPhaseProposed || p.DecidedBy != "" || p.DecidedAt != nil {
		t.Fatalf("after rollback: %+v", p)
	}
	// And the retry works.
	if _, err := store.ConfirmProposal(ctx, projectID, teamID, msg.ID, human); err != nil {
		t.Fatalf("retry confirm: %v", err)
	}
}

// TestListProposalsReadSide — the story-6 read (ISI-4930): every card in the thread lists with its
// DURABLE phase in transcript order (the transcript itself stays phase-less — dismissals and
// executions are otherwise un-derivable after a reload), and a foreign Team's list read sees
// nothing (the team filter empties the result; the thread read remains the existence gate).
func TestListProposalsReadSide(t *testing.T) {
	store, projectID, teamID, threadID, agent, human := proposalFixtures(t)
	ctx := context.Background()

	m1, err := store.PostProposal(ctx, projectID, teamID, threadID, agent, "stay proposed",
		ProposalPayload{Action: ProposalActionCreateTicket, Title: "one"}, nil)
	if err != nil {
		t.Fatalf("post 1: %v", err)
	}
	m2, err := store.PostProposal(ctx, projectID, teamID, threadID, agent, "dismiss me",
		ProposalPayload{Action: ProposalActionAssignAgent, TicketID: "wi-2", AssigneeAgentID: "a:kimi"}, nil)
	if err != nil {
		t.Fatalf("post 2: %v", err)
	}
	if err := store.DismissProposal(ctx, projectID, teamID, m2.ID, human); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	m3, err := store.PostProposal(ctx, projectID, teamID, threadID, agent, "execute me",
		ProposalPayload{Action: ProposalActionPartyRun, Title: "three"}, nil)
	if err != nil {
		t.Fatalf("post 3: %v", err)
	}
	if _, err := store.ConfirmProposal(ctx, projectID, teamID, m3.ID, human); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	result, _ := json.Marshal(map[string]any{"action": "party_run", "workItemId": "wi-3"})
	if _, err := store.CompleteProposal(ctx, projectID, teamID, m3.ID, human, result, "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	list, err := store.ListProposals(ctx, projectID, teamID, threadID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 proposals, got %d: %+v", len(list), list)
	}
	wantPhases := map[string]string{m1.ID.String(): ProposalPhaseProposed, m2.ID.String(): ProposalPhaseDismissed, m3.ID.String(): ProposalPhaseExecuted}
	for _, p := range list {
		if p.Message.Kind != KindProposal {
			t.Fatalf("non-proposal row in list: %+v", p.Message)
		}
		if wantPhases[p.Message.ID.String()] != p.Phase {
			t.Fatalf("card %s phase: got %q want %q", p.Message.ID, p.Phase, wantPhases[p.Message.ID.String()])
		}
	}
	if list[0].Message.ID != m1.ID || list[1].Message.ID != m2.ID || list[2].Message.ID != m3.ID {
		t.Fatalf("list not in transcript order: %s, %s, %s", list[0].Message.ID, list[1].Message.ID, list[2].Message.ID)
	}
	if list[1].DecidedBy != human.Principal || list[2].Payload.Action != ProposalActionPartyRun {
		t.Fatalf("decision provenance / payload lost: %+v %+v", list[1], list[2])
	}

	// Cross-team: the list is empty for a foreign team — no existence leak, no rows.
	foreign, err := store.ListProposals(ctx, projectID, uuid.New(), threadID)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("cross-team list: want empty nil-err, got %d %v", len(foreign), err)
	}
}

// TestProposalTenancyFence — a proposal in another Team's room is invisible (404-not-403, AC5):
// get, confirm, and dismiss all hide existence cross-tenant.
func TestProposalTenancyFence(t *testing.T) {
	store, projectID, teamID, threadID, agent, _ := proposalFixtures(t)
	ctx := context.Background()

	msg, err := store.PostProposal(ctx, projectID, teamID, threadID, agent, "Private room proposal",
		ProposalPayload{Action: ProposalActionCreateTicket, Title: "t"}, nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	otherTeam := uuid.New()
	outsider := AuthorContext{Principal: "user:eve", TeamID: otherTeam}
	if _, err := store.GetProposal(ctx, projectID, otherTeam, msg.ID); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("cross-team get: want ErrProposalNotFound, got %v", err)
	}
	if _, err := store.ConfirmProposal(ctx, projectID, otherTeam, msg.ID, outsider); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("cross-team confirm: want ErrProposalNotFound, got %v", err)
	}
	if err := store.DismissProposal(ctx, projectID, otherTeam, msg.ID, outsider); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("cross-team dismiss: want ErrProposalNotFound, got %v", err)
	}
	// The card is untouched for its own team.
	p, err := store.GetProposal(ctx, projectID, teamID, msg.ID)
	if err != nil || p.Phase != ProposalPhaseProposed {
		t.Fatalf("owner view after outsider probes: %+v %v", p, err)
	}
}

// TestProposalRetractedCannotBeDecided — a retracted proposal card is gone from the decision
// surface: get/confirm answer 404.
func TestProposalRetractedCannotBeDecided(t *testing.T) {
	store, projectID, teamID, threadID, agent, human := proposalFixtures(t)
	ctx := context.Background()

	msg, err := store.PostProposal(ctx, projectID, teamID, threadID, agent, "Retract me",
		ProposalPayload{Action: ProposalActionCreateTicket, Title: "t"}, nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := store.Retract(ctx, projectID, teamID, threadID, msg.ID, agent); err != nil {
		t.Fatalf("retract: %v", err)
	}
	if _, err := store.GetProposal(ctx, projectID, teamID, msg.ID); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("retracted get: want ErrProposalNotFound, got %v", err)
	}
	if _, err := store.ConfirmProposal(ctx, projectID, teamID, msg.ID, human); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("retracted confirm: want ErrProposalNotFound, got %v", err)
	}
}

// TestProposalKindCheckConstraint — the widened kind CHECK (0025) admits 'proposal' on insert.
func TestProposalKindCheckConstraint(t *testing.T) {
	store, projectID, teamID, threadID, agent, _ := proposalFixtures(t)
	msg, err := store.PostProposal(context.Background(), projectID, teamID, threadID, agent, "kind check",
		ProposalPayload{Action: ProposalActionCreateTicket, Title: "t"}, nil)
	if err != nil {
		t.Fatalf("proposal kind rejected by the CHECK constraint: %v", err)
	}
	if msg.CreatedAt.IsZero() || time.Since(msg.CreatedAt) > time.Minute {
		t.Fatalf("created_at not server-stamped: %+v", msg)
	}
}
