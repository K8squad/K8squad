//go:build discussion_integration

// Integration coverage for the ISI-5616 party-round message-wire enrichment (ISI-5613 Gap 1; ADR-0027
// addendum) against a REAL Postgres. Build-tag gated with the rest of the discussion integration lane;
// CI provisions Postgres and runs `go test -tags=discussion_integration ./internal/discussion/...`, and
// the suite SKIPS when DATABASE_URL is unset.
//
// It proves the contract ISI-5613's console depends on: GetThread tags each party message with its
// session id + 1-based round + kind — the party_start opener (round 0 / "opener"), the facilitator round
// post, and a dispatched voice reply (round recovered via run → coord.claim → mention_dispatch →
// party_round) — while ordinary (non-party) messages carry no linkage. It also exercises the durable
// ledger write SetRoundFacilitatorMessage performs (the round row GetThread reads back).
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

// applyPartyReadMigrations lays the full chain GetThread's party enrichment needs onto a clean pair of
// schemas: coord (work_item/claim) + discussion (base → message fields → party_session → round-facilitator
// columns → the 0035 party_round ledger) + the mention_dispatch ledger. Mirrors the file-locating pattern
// of applyMigration / the ISI-5617 read suite.
func applyPartyReadMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS discussion CASCADE; DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset schemas: %v", err)
	}
	names := []string{
		"0001_coord_schema.sql",
		"0004_discussion_schema.sql",
		"0024_discussion_message_fields.sql",
		"0026_discussion_project_id_text.sql",
		"0027_dispatch_on_mention_run_minting.sql",
		"0033_discussion_party_session.sql",
		"0034_discussion_party_round_facilitator.sql",
		"0035_discussion_party_round.sql",
	}
	for _, name := range names {
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
			t.Fatalf("could not locate %s (tried %v); set DISCUSSION_MIGRATIONS_DIR", name, candidates)
		}
		if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("the shipped migration %s failed to apply against real Postgres: %v", name, err)
		}
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = db.ExecContext(cctx, `DROP SCHEMA IF EXISTS discussion CASCADE; DROP SCHEMA IF EXISTS coord CASCADE`)
	})
}

// TestGetThreadStampsPartyRoundLinkage walks a one-round party debate's durable state into Postgres and
// asserts GetThread surfaces the round + session + kind on the opener, the facilitator post, and a voice
// reply — and leaves an ordinary message untagged.
func TestGetThreadStampsPartyRoundLinkage(t *testing.T) {
	db := openTestDB(t)
	applyPartyReadMigrations(t, db)
	store := NewStore(db)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const projectSlug = "team-a/proj-x"
	team := uuid.New()
	threadID := uuid.New()
	coordProject := uuid.New() // coord.work_item.project_id is a uuid; unrelated to the room slug

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	// Thread (scoped to projectSlug + team — the tenancy GetThread filters on).
	exec(`INSERT INTO discussion.thread (id, project_id, team_id, title, created_by)
	      VALUES ($1,$2,$3,'storage debate','henrik')`, threadID, projectSlug, team)

	// (a) party_start opener (human).
	openerID := uuid.New()
	exec(`INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
	      VALUES ($1,$2,'henrik','let''s debate storage','party','party_start')`, openerID, threadID)

	// Active session at round 1 (the round the facilitator is about to dispatch), opener as topic.
	sessID := uuid.New()
	exec(`INSERT INTO discussion.party_session
	        (id, thread_id, project_id, team_id, started_by, topic_message_id,
	         round, max_rounds, max_voices_per_round, paid_run_budget, phase)
	      VALUES ($1,$2,$3,$4,'henrik',$5, 1, 3, 3, 12, 'active')`,
		sessID, threadID, projectSlug, team, openerID)

	// (b) facilitator round-1 @-mention post (coordinator).
	facID := uuid.New()
	exec(`INSERT INTO discussion.message (id, thread_id, author_principal, author_agent_id, body, audience, kind)
	      VALUES ($1,$2,'winston','winston','Round 1 — @sam weigh in','party','text')`, facID, threadID)

	// SetRoundFacilitatorMessage is the seam under test: it stamps current_round_message_id AND writes
	// the durable party_round ledger row GetThread reads back.
	if _, won, err := store.SetRoundFacilitatorMessage(ctx, sessID, 1, facID); err != nil || !won {
		t.Fatalf("SetRoundFacilitatorMessage: won=%v err=%v (want won=true, nil)", won, err)
	}

	// (c) a dispatched voice: coord.work_item + claim(run_id) + mention_dispatch keyed under the
	//     facilitator message, then the voice's reply message carrying that run id.
	voiceWI := uuid.New()
	voiceRun := uuid.New()
	exec(`INSERT INTO coord.work_item (id, project_id, title, created_by) VALUES ($1,$2,'voice run','dispatch')`, voiceWI, coordProject)
	// coord.work_item auto-provisions its single claim row via the provision_claim trigger (0001), so set
	// the run_id on that existing row rather than INSERTing a second (which collides on claim_pkey).
	exec(`UPDATE coord.claim SET run_id = $2 WHERE work_item_id = $1`, voiceWI, voiceRun)
	exec(`INSERT INTO discussion.mention_dispatch (message_id, agent_name, project_id, thread_id, hop_depth, work_item_id)
	      VALUES ($1,'sam',$2,$3,1,$4)`, facID, projectSlug, threadID, voiceWI)
	voiceMsgID := uuid.New()
	exec(`INSERT INTO discussion.message (id, thread_id, author_principal, author_agent_id, author_run_id, body, audience, kind)
	      VALUES ($1,$2,'sam','sam',$3,'I disagree — object store wins','party','text')`, voiceMsgID, threadID, voiceRun.String())

	// A plain, non-party message (ordinary human post) — must stay untagged.
	plainID := uuid.New()
	exec(`INSERT INTO discussion.message (id, thread_id, author_principal, body, audience, kind)
	      VALUES ($1,$2,'henrik','unrelated aside','party','text')`, plainID, threadID)

	// Read the thread and index the (flattened) messages by id.
	th, err := store.GetThread(ctx, projectSlug, team, threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	byID := make(map[uuid.UUID]Message)
	for _, m := range flattenMessages(th.Messages) {
		byID[m.ID] = m
	}

	assertAnchor := func(label string, id uuid.UUID, wantRound int, wantKind string) {
		t.Helper()
		m, ok := byID[id]
		if !ok {
			t.Fatalf("%s: message %s absent from GetThread", label, id)
		}
		if m.PartySessionID == nil || *m.PartySessionID != sessID {
			t.Errorf("%s: partySessionId = %v, want %s", label, m.PartySessionID, sessID)
		}
		if m.PartyRound == nil || *m.PartyRound != wantRound {
			t.Errorf("%s: partyRound = %v, want %d", label, m.PartyRound, wantRound)
		}
		if m.PartyRoundKind != wantKind {
			t.Errorf("%s: partyRoundKind = %q, want %q", label, m.PartyRoundKind, wantKind)
		}
	}
	assertAnchor("opener", openerID, 0, "opener")
	assertAnchor("facilitator", facID, 1, "round")
	assertAnchor("voice", voiceMsgID, 1, "round")

	// The ordinary message carries no party linkage.
	if plain, ok := byID[plainID]; !ok {
		t.Fatalf("plain message absent from GetThread")
	} else if plain.PartySessionID != nil || plain.PartyRound != nil || plain.PartyRoundKind != "" {
		t.Errorf("plain message tagged: sessionID=%v round=%v kind=%q, want all empty",
			plain.PartySessionID, plain.PartyRound, plain.PartyRoundKind)
	}

	// The ledger write is durable + idempotent: a replayed stamp for the same round no-ops (won=false),
	// never a second row / error.
	if _, won, err := store.SetRoundFacilitatorMessage(ctx, sessID, 1, facID); err != nil || won {
		t.Fatalf("replayed SetRoundFacilitatorMessage: won=%v err=%v (want won=false, nil)", won, err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM discussion.party_round WHERE session_id=$1 AND round=1`, sessID).Scan(&rows); err != nil {
		t.Fatalf("count party_round: %v", err)
	}
	if rows != 1 {
		t.Errorf("party_round rows for (session,1) = %d, want 1", rows)
	}
}
