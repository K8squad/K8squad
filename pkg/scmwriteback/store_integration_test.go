//go:build integration

// Integration regression proof for the PR write-back pending query (ISI-6409).
// coord.work_item.team_id is a nullable uuid; pendingPRQuery coalesces it
// against the empty string on the wire to the Go string scan target. Before
// the ::text cast, Postgres resolved the ” literal to uuid and evaluated
// ”::uuid for every matching row whose team_id is NULL, failing the WHOLE
// PR leg with SQLSTATE 22P02 (invalid input syntax for type uuid: "") and
// flipping the Project to SyncReady=False reason=PRWriteBackError - blocking
// mirror sync on every reconcile. This lane seeds one NULL-team_id and one
// set-team_id github-labelled work item, each with a succeeded run_terminal
// audit row carrying a source_branch, and asserts PendingPRWriteBacks returns
// both rows with TeamID "" for the NULL row. Run with DATABASE_URL set:
//
//	go test -tags=integration ./pkg/scmwriteback/ -run TestPendingPR
package scmwriteback

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

const isi6409Project = "22222222-2222-2222-2222-222222222222"

// isi6409DB provisions the coord schema subset pendingPRQuery reads
// (work_item + claim + audit_log; 0018 adds claim.assignee_agent, 0020 adds
// work_item.labels) against the DATABASE_URL scratch instance, mirroring the
// pkg/events integration harness.
func isi6409DB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset - skipping scmwriteback store integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "DROP SCHEMA IF EXISTS coord CASCADE"); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	for _, f := range []string{"0001_coord_schema.sql", "0018_claim_assignee.sql", "0020_work_item_create_fields.sql"} {
		sqlText, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.ExecContext(ctx, string(sqlText)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return db
}

// seedTerminalRun inserts a github-labelled work item (teamID "" stores NULL)
// plus its succeeded run_terminal audit row carrying the source_branch the PR
// leg keys on, and stamps the auto-provisioned claim row's agent attribution.
func seedTerminalRun(ctx context.Context, t *testing.T, db *sql.DB, title, issueRef, teamID, agent, branch string) string {
	t.Helper()
	var id string
	err := db.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, team_id, title, state, created_by, labels)
		VALUES ($1::uuid, NULLIF($2, '')::uuid, $3, 'done', 'seed',
		        ARRAY['ksquad.github.issue=' || $4])
		RETURNING id`,
		isi6409Project, teamID, title, issueRef).Scan(&id)
	if err != nil {
		t.Fatalf("seed work item %s: %v", title, err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO coord.audit_log (work_item_id, run_id, event_type, to_state, principal, payload)
		VALUES ($1::uuid, $2::uuid, 'run_terminal', 'succeeded', 'seed',
		        jsonb_build_object('source_branch', $3::text))`,
		id, id, branch); err != nil {
		t.Fatalf("seed terminal %s: %v", title, err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE coord.claim SET assignee_agent = $1 WHERE work_item_id = $2::uuid", agent, id); err != nil {
		t.Fatalf("stamp claim %s: %v", title, err)
	}
	return id
}

// ISI-6409: a matching row with NULL team_id must yield TeamID "" - the bare
// COALESCE(uuid_col, ”) form instead failed the entire query with 22P02.
func TestPendingPRWriteBacks_NullTeamIDDoesNotCrash(t *testing.T) {
	db := isi6409DB(t)
	ctx := context.Background()

	nullID := seedTerminalRun(ctx, t, db, "null-team item", "acme/repo#1", "", "sam", "agent/feat-one")
	const team = "33333333-3333-3333-3333-333333333333"
	teamID := seedTerminalRun(ctx, t, db, "team item", "acme/repo#2", team, "kim", "agent/feat-two")

	s, err := NewSQLStore(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	pending, err := s.PendingPRWriteBacks(ctx, isi6409Project)
	if err != nil {
		t.Fatalf("PendingPRWriteBacks: %v (expected no uuid-cast failure)", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending PR rows, got %d: %+v", len(pending), pending)
	}
	byWI := map[string]PRPending{}
	for _, p := range pending {
		byWI[p.WorkItemID] = p
	}
	if got := byWI[nullID].TeamID; got != "" {
		t.Errorf("NULL team_id row: TeamID = %q, want empty", got)
	}
	if got := byWI[teamID].TeamID; got != team {
		t.Errorf("set team_id row: TeamID = %q, want %q", got, team)
	}
	if got := byWI[nullID].AgentName; got != "sam" {
		t.Errorf("NULL team_id row: AgentName = %q, want sam", got)
	}
	if got := byWI[nullID].SourceBranch; got != "agent/feat-one" {
		t.Errorf("NULL team_id row: SourceBranch = %q, want agent/feat-one", got)
	}
	if got := byWI[nullID].IssueRef; got != "acme/repo#1" {
		t.Errorf("NULL team_id row: IssueRef = %q, want acme/repo#1", got)
	}
}
