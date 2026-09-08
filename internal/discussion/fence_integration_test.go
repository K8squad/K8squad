//go:build discussion_integration

// The DB-backed half of the discussion-room fence suite (see fence_test.go for the authority doc
// comment and the tag-free AC1 allow-list). These assertions need a REAL Postgres and are gated to
// the `discussion_integration` lane; when DATABASE_URL is unset they SKIP (mirroring the shipped
// integration_test.go). They apply the SHIPPED migrations — not inline DDL — so a drift between the
// schema and this guarantee goes RED here.
//
// Shared helpers (openTestDB, applyMigration, newServer, mustReq, do, decode, headerAuth, postResp)
// live in integration_test.go in this same package + build tag. AC3/AC5 here CONSOLIDATE the former
// TestServerStampsAuthorIgnoringForgedBody / TestCrossTeamReadsAreEmpty (removed from
// integration_test.go) so the whole fence lives in one discoverable place (AC7).
package discussion

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ─────────────────────────────────────────────────────────────────────────────
// AC1 (live form) — no custody column, asserted against the APPLIED schema.
// ─────────────────────────────────────────────────────────────────────────────

// TestDiscussionFenceSchemaIntrospection is AC1 against a live Postgres: it applies the shipped 0004
// migration and queries information_schema.columns, asserting the applied discussion.thread /
// discussion.message column sets are EXACTLY the fence allow-list (allowedThreadColumns /
// allowedMessageColumns from fence_test.go) and that no column name carries a forbidden custody
// token. This is the machine form of ADR-0019 AC4 against the real DB — the tag-free
// TestDiscussionFenceSchemaHasNoCustodyColumn proves the same over the migration text so the unit
// lane still guards the fence without Postgres.
func TestDiscussionFenceSchemaIntrospection(t *testing.T) {
	db := openTestDB(t)
	applyMigration(t, db)

	cases := []struct {
		table   string
		allowed []string
	}{
		{"thread", allowedThreadColumns},
		{"message", allowedMessageColumns},
	}
	for _, c := range cases {
		got := introspectColumns(t, db, "discussion", c.table)
		assertColumnAllowList(t, "discussion."+c.table, got, c.allowed)
		for _, col := range got {
			assertNoForbiddenToken(t, "discussion."+c.table, col)
		}
	}
}

func introspectColumns(t *testing.T, db *sql.DB, schema, table string) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY column_name`, schema, table)
	if err != nil {
		t.Fatalf("introspect %s.%s: %v", schema, table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan column_name: %v", err)
		}
		cols = append(cols, strings.ToLower(c))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(cols) == 0 {
		t.Fatalf("information_schema shows zero columns for %s.%s — schema not applied?", schema, table)
	}
	return cols
}

// ─────────────────────────────────────────────────────────────────────────────
// AC2 — a discussion write moves NO coordination state.
// ─────────────────────────────────────────────────────────────────────────────

// TestDiscussionFenceWriteMovesNoCoordState is AC2 / R13 / FR-B3: with a seeded coord custody
// fixture (a work_item whose claim is HELD), it snapshots every coord custody row, runs a FULL
// discussion write cycle through the Store (open thread → post → reply → retract), then re-snapshots
// and asserts byte-identical. Proves no discussion operation can transfer custody — the room cannot
// be a handoff. If this ever fails, a discussion write reached the coord spine: a fence-breach bug.
func TestDiscussionFenceWriteMovesNoCoordState(t *testing.T) {
	db := openTestDB(t)
	applyCoordMigration(t, db) // seeds the coord schema fresh
	applyMigration(t, db)      // discussion schema

	ctx := context.Background()

	// Seed a coord work item; the 0001 trigger auto-provisions its claim row. Then simulate custody
	// (holder + non-zero fence) so the snapshot has real state to protect.
	var workItemID uuid.UUID
	if err := db.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, title, created_by)
		VALUES ($1, 'fence fixture', 'principal:seed') RETURNING id`, uuid.New()).Scan(&workItemID); err != nil {
		t.Fatalf("seed work_item: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE coord.claim SET holder_principal = 'principal:holder', run_id = $2,
		       fence_token = 7, acquired_at = now(), renewed_at = now()
		WHERE work_item_id = $1`, workItemID, uuid.New()); err != nil {
		t.Fatalf("simulate held claim: %v", err)
	}

	before := snapshotCoordCustody(t, db)

	// Full discussion write cycle through the production Store.
	store := NewStore(db)
	team := uuid.New()
	project := uuid.New()
	auth := AuthorContext{Principal: "principal:writer", TeamID: team}

	th, err := store.OpenThread(ctx, project, auth, "does a write move custody?", "opening message")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	msg, err := store.PostMessage(ctx, project, team, th.ID, auth, "second message", nil)
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	reply, err := store.PostMessage(ctx, project, team, th.ID, auth, "a reply", &msg.ID)
	if err != nil {
		t.Fatalf("PostMessage(reply): %v", err)
	}
	if err := store.Retract(ctx, project, team, th.ID, reply.ID, auth); err != nil {
		t.Fatalf("Retract: %v", err)
	}

	after := snapshotCoordCustody(t, db)

	if before != after {
		t.Errorf("AC2 FENCE BREACH: a discussion write cycle mutated coord custody state.\n--- before ---\n%s\n--- after ---\n%s",
			before, after)
	}
}

// snapshotCoordCustody renders a canonical text dump of every coord row that carries custody
// semantics: the full coord.claim table (holder/run/fence/lease/timestamps) and the custody-relevant
// columns of coord.work_item (state + blocked_reason). Ordered so equality is deterministic.
func snapshotCoordCustody(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder

	b.WriteString("== coord.claim ==\n")
	claimRows, err := db.Query(`
		SELECT work_item_id, COALESCE(holder_principal,''), COALESCE(run_id::text,''),
		       fence_token, COALESCE(lease_expires_at::text,''), COALESCE(acquired_at::text,''),
		       COALESCE(renewed_at::text,''), COALESCE(initiated_by_user_id::text,'')
		FROM coord.claim ORDER BY work_item_id`)
	if err != nil {
		t.Fatalf("snapshot coord.claim: %v", err)
	}
	defer claimRows.Close()
	for claimRows.Next() {
		var wi, holder, run, lease, acq, renewed, initiator string
		var fence int64
		if err := claimRows.Scan(&wi, &holder, &run, &fence, &lease, &acq, &renewed, &initiator); err != nil {
			t.Fatalf("scan coord.claim: %v", err)
		}
		fmt.Fprintf(&b, "%s|%s|%s|%d|%s|%s|%s|%s\n", wi, holder, run, fence, lease, acq, renewed, initiator)
	}
	if err := claimRows.Err(); err != nil {
		t.Fatalf("coord.claim rows: %v", err)
	}

	b.WriteString("== coord.work_item(state) ==\n")
	wiRows, err := db.Query(`
		SELECT id, state, COALESCE(blocked_reason,'') FROM coord.work_item ORDER BY id`)
	if err != nil {
		t.Fatalf("snapshot coord.work_item: %v", err)
	}
	defer wiRows.Close()
	for wiRows.Next() {
		var id, state, blocked string
		if err := wiRows.Scan(&id, &state, &blocked); err != nil {
			t.Fatalf("scan coord.work_item: %v", err)
		}
		fmt.Fprintf(&b, "%s|%s|%s\n", id, state, blocked)
	}
	if err := wiRows.Err(); err != nil {
		t.Fatalf("coord.work_item rows: %v", err)
	}
	return b.String()
}

// applyCoordMigration applies the SHIPPED 0001 coord migration into a clean `coord` schema (mirrors
// applyMigration's discipline for discussion). CI provisions a dedicated Postgres per the ephemeral
// PG-service model, so a schema reset is the established convention in this package.
func applyCoordMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset coord schema: %v", err)
	}
	candidates := []string{
		filepath.Join("..", "..", "db", "migrations", "0001_coord_schema.sql"),
		filepath.Join("db", "migrations", "0001_coord_schema.sql"),
	}
	if d := os.Getenv("DISCUSSION_MIGRATIONS_DIR"); d != "" {
		candidates = append([]string{filepath.Join(d, "0001_coord_schema.sql")}, candidates...)
	}
	var sqlBytes []byte
	var err error
	for _, c := range candidates {
		if sqlBytes, err = os.ReadFile(c); err == nil {
			break
		}
	}
	if sqlBytes == nil {
		t.Fatalf("could not locate 0001_coord_schema.sql (tried %v); set DISCUSSION_MIGRATIONS_DIR", candidates)
	}
	if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("the shipped coord migration failed to apply: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = db.ExecContext(cctx, `DROP SCHEMA IF EXISTS coord CASCADE`)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// AC3 — provenance is always server-stamped; a forged body author never lands.
// (Consolidates & renames the former TestServerStampsAuthorIgnoringForgedBody.)
// ─────────────────────────────────────────────────────────────────────────────

// TestDiscussionFenceProvenanceServerStamped is AC3 against real Postgres: an authenticated caller
// posts a body that ALSO smuggles forged author_/created_by/team_id/project_id values; the STORED
// row must carry the AUTHENTICATED principal/team/project (from context + URL), the attacker values
// must appear NOWHERE, and agent-vs-human must derive from the context's AgentID — not the body.
func TestDiscussionFenceProvenanceServerStamped(t *testing.T) {
	db := openTestDB(t)
	applyMigration(t, db)
	srv := newServer(db)
	defer srv.Close()

	teamReal := uuid.New()
	teamAttacker := uuid.New()
	projectReal := uuid.New()
	projectAttacker := uuid.New()

	// Human post (no X-Test-Agent), authenticated as principal:real / teamReal / projectReal, but the
	// body smuggles a full set of attacker-controlled provenance fields.
	forged := fmt.Sprintf(`{"title":"design sync","body":"kickoff",`+
		`"author_principal":"principal:VICTIM","authorId":"%s","author_agent_id":"agent:ATTACKER",`+
		`"author_run_id":"run:ATTACKER","authorType":"human","authorName":"attacker",`+
		`"created_by":"principal:VICTIM","team_id":"%s","project_id":"%s","teamId":"%s"}`,
		uuid.NewString(), teamAttacker, projectAttacker, teamAttacker)

	req := mustReq(t, http.MethodPost,
		srv.URL+"/api/projects/"+projectReal.String()+"/discussion/threads",
		forged, "principal:real", teamReal.String(), "", "")
	res := do(t, req)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("openThread: got %d, want 201", res.StatusCode)
	}
	var opened postResp
	decode(t, res, &opened)
	if opened.CreatedBy != "principal:real" {
		t.Errorf("thread created_by: got %q, want principal:real (forged body author must be ignored)", opened.CreatedBy)
	}
	if len(opened.Messages) != 1 || opened.Messages[0].AuthorPrincipal != "principal:real" {
		t.Errorf("first message author: got %+v, want principal:real", opened.Messages)
	}

	// Inspect the STORED row directly: authenticated identity/tenancy landed; nothing attacker did.
	var (
		author, createdBy, storedTeam, storedProject string
		agentID, runID                               sql.NullString
	)
	if err := db.QueryRow(`
		SELECT m.author_principal, t.created_by, t.team_id::text, t.project_id::text,
		       m.author_agent_id, m.author_run_id
		FROM discussion.message m JOIN discussion.thread t ON t.id = m.thread_id
		WHERE t.id = $1`, opened.ID).Scan(&author, &createdBy, &storedTeam, &storedProject, &agentID, &runID); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if author != "principal:real" || createdBy != "principal:real" {
		t.Errorf("stored provenance: author=%q created_by=%q, want principal:real", author, createdBy)
	}
	if storedTeam != teamReal.String() {
		t.Errorf("stored team_id: got %q, want authenticated %q", storedTeam, teamReal)
	}
	if storedProject != projectReal.String() {
		t.Errorf("stored project_id: got %q, want URL-scoped %q", storedProject, projectReal)
	}
	// Human context ⇒ author_agent_id / author_run_id NULL. A forged agent id in the body must NOT
	// flip the row to agent-authored (agent-vs-human is DERIVED from context, not the body).
	if agentID.Valid {
		t.Errorf("AC3 VIOLATION: forged author_agent_id landed (%q) — human post must stay human", agentID.String)
	}
	if runID.Valid {
		t.Errorf("AC3 VIOLATION: forged author_run_id landed (%q)", runID.String)
	}

	// Belt-and-suspenders: no attacker value survives ANYWHERE in the row.
	for _, poison := range []string{"VICTIM", "ATTACKER", teamAttacker.String(), projectAttacker.String()} {
		if strings.Contains(author, poison) || strings.Contains(createdBy, poison) ||
			strings.Contains(storedTeam, poison) || strings.Contains(storedProject, poison) ||
			strings.Contains(agentID.String, poison) || strings.Contains(runID.String, poison) {
			t.Errorf("AC3 VIOLATION: forged value %q reached the stored row", poison)
		}
	}

	// Positive: an AGENT context (X-Test-Agent + X-Test-Run) DOES derive agent-authored — proving the
	// derivation is driven by context, exactly the channel the body could not forge above.
	agentReq := mustReq(t, http.MethodPost,
		srv.URL+"/api/projects/"+projectReal.String()+"/discussion/threads",
		`{"title":"agent thread","body":"from a run"}`, "principal:agent", teamReal.String(),
		"agent:coordinator", "run:123")
	var agentOpened postResp
	decode(t, do(t, agentReq), &agentOpened) //nolint:bodyclose // decode() closes res.Body
	var gotAgent, gotRun sql.NullString
	if err := db.QueryRow(`SELECT author_agent_id, author_run_id FROM discussion.message WHERE thread_id = $1`,
		agentOpened.ID).Scan(&gotAgent, &gotRun); err != nil {
		t.Fatalf("read agent-post provenance: %v", err)
	}
	if !gotAgent.Valid || gotAgent.String != "agent:coordinator" {
		t.Errorf("agent post author_agent_id: got %v, want agent:coordinator (context-derived)", gotAgent)
	}
	if !gotRun.Valid || gotRun.String != "run:123" {
		t.Errorf("agent post author_run_id: got %v, want run:123 (context-derived)", gotRun)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC4 — an unattributed/unscoped row is un-representable; retract is the only mutation.
// ─────────────────────────────────────────────────────────────────────────────

// TestDiscussionFenceUnattributedRowUnrepresentable is AC4 against real Postgres: NOT NULL rejects
// any thread/message missing its scope or provenance, and the append-only triggers reject every
// body/author UPDATE, hard DELETE and TRUNCATE — leaving the one-way soft-retract as the ONLY
// permitted mutation. Mirrors the shipped trigger discipline, consolidated into the fence authority.
func TestDiscussionFenceUnattributedRowUnrepresentable(t *testing.T) {
	db := openTestDB(t)
	applyMigration(t, db)
	ctx := context.Background()

	project := uuid.New()
	team := uuid.New()

	// A valid thread + message to attack (inserted with full scope + provenance).
	var threadID, messageID uuid.UUID
	if err := db.QueryRowContext(ctx, `
		INSERT INTO discussion.thread (project_id, team_id, title, created_by)
		VALUES ($1, $2, 'title', 'principal:real') RETURNING id`, project, team).Scan(&threadID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		INSERT INTO discussion.message (thread_id, author_principal, body)
		VALUES ($1, 'principal:real', 'hello') RETURNING id`, threadID).Scan(&messageID); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	// (a) NOT NULL: unscoped / unattributed rows are un-representable.
	expectRejected(t, db, "thread NULL project_id",
		`INSERT INTO discussion.thread (project_id, team_id, title, created_by) VALUES (NULL, $1, 't', 'p')`, team)
	expectRejected(t, db, "thread NULL team_id",
		`INSERT INTO discussion.thread (project_id, team_id, title, created_by) VALUES ($1, NULL, 't', 'p')`, project)
	expectRejected(t, db, "thread NULL created_by",
		`INSERT INTO discussion.thread (project_id, team_id, title, created_by) VALUES ($1, $2, 't', NULL)`, project, team)
	expectRejected(t, db, "message NULL author_principal",
		`INSERT INTO discussion.message (thread_id, author_principal, body) VALUES ($1, NULL, 'b')`, threadID)
	expectRejected(t, db, "message NULL body",
		`INSERT INTO discussion.message (thread_id, author_principal, body) VALUES ($1, 'principal:real', NULL)`, threadID)

	// (b) Append-only: body/author rewrites, hard delete, truncate all rejected on message.
	expectRejected(t, db, "message body UPDATE",
		`UPDATE discussion.message SET body = 'tampered' WHERE id = $1`, messageID)
	expectRejected(t, db, "message author UPDATE",
		`UPDATE discussion.message SET author_principal = 'principal:VICTIM' WHERE id = $1`, messageID)
	expectRejected(t, db, "message hard DELETE",
		`DELETE FROM discussion.message WHERE id = $1`, messageID)
	expectRejected(t, db, "message TRUNCATE", `TRUNCATE discussion.message`)

	// (c) Append-only on thread: immutable once opened.
	expectRejected(t, db, "thread title UPDATE",
		`UPDATE discussion.thread SET title = 'tampered' WHERE id = $1`, threadID)
	expectRejected(t, db, "thread hard DELETE",
		`DELETE FROM discussion.thread WHERE id = $1`, threadID)
	expectRejected(t, db, "thread TRUNCATE", `TRUNCATE discussion.thread`)

	// (d) The ONE permitted mutation: the one-way soft-retract must SUCCEED.
	if _, err := db.ExecContext(ctx,
		`UPDATE discussion.message SET invalidated_at = now() WHERE id = $1`, messageID); err != nil {
		t.Errorf("soft-retract must be the one allowed mutation, but it was rejected: %v", err)
	}
	// ...and it is one-way: re-validating (ts → NULL) is rejected.
	expectRejected(t, db, "un-retract (invalidated_at → NULL)",
		`UPDATE discussion.message SET invalidated_at = NULL WHERE id = $1`, messageID)
}

// expectRejected runs a statement expected to be rejected by NOT NULL or an append-only trigger and
// fails the test if it unexpectedly succeeds.
func expectRejected(t *testing.T, db *sql.DB, desc, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err == nil {
		t.Errorf("AC4 FENCE BREACH: %q was accepted but must be rejected (NOT NULL / append-only)", desc)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC5 — cross-tenant reads are empty and 404-not-403.
// (Consolidates & renames the former TestCrossTeamReadsAreEmpty.)
// ─────────────────────────────────────────────────────────────────────────────

// TestDiscussionFenceCrossTenantReadsEmpty is AC5 against real Postgres: Team A opens a thread; Team
// B (a different Team scope) reading the SAME Project sees zero threads and gets 404 (never 403) on
// the specific thread — absence, not "forbidden", so a cross-tenant probe cannot distinguish them.
func TestDiscussionFenceCrossTenantReadsEmpty(t *testing.T) {
	db := openTestDB(t)
	applyMigration(t, db)
	srv := newServer(db)
	defer srv.Close()

	teamA := uuid.New()
	teamB := uuid.New()
	project := uuid.New() // same Project id probed by both teams

	req := mustReq(t, http.MethodPost, srv.URL+"/api/projects/"+project.String()+"/discussion/threads",
		`{"title":"team A only","body":"secret"}`, "principal:a", teamA.String(), "", "")
	res := do(t, req)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("teamA openThread: got %d, want 201", res.StatusCode)
	}
	var opened postResp
	decode(t, res, &opened)

	// Team A sees its own thread; Team B sees zero.
	if n := listThreadCount(t, srv.URL, project, "principal:a", teamA); n != 1 {
		t.Fatalf("teamA list: got %d threads, want 1", n)
	}
	if n := listThreadCount(t, srv.URL, project, "principal:b", teamB); n != 0 {
		t.Errorf("AC5 VIOLATION: teamB saw %d threads from teamA's Project, want 0", n)
	}

	// Team B fetches Team A's specific thread — must be 404-not-403.
	getReq := mustReq(t, http.MethodGet,
		srv.URL+"/api/projects/"+project.String()+"/discussion/threads/"+opened.ID.String(),
		"", "principal:b", teamB.String(), "", "")
	getRes := do(t, getReq)
	defer getRes.Body.Close()
	if getRes.StatusCode != http.StatusNotFound {
		t.Errorf("AC5: teamB GET teamA thread: got %d, want 404 (not 403)", getRes.StatusCode)
	}
}

func listThreadCount(t *testing.T, base string, project uuid.UUID, principal string, team uuid.UUID) int {
	t.Helper()
	req := mustReq(t, http.MethodGet, base+"/api/projects/"+project.String()+"/discussion/threads",
		"", principal, team.String(), "", "")
	var threads []json.RawMessage
	decode(t, do(t, req), &threads) //nolint:bodyclose // decode() closes res.Body
	return len(threads)
}
