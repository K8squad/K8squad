//go:build search_integration

// PostgresSearcher integration test against a REAL Postgres with the SHIPPED
// migrations (story 8.18 / ISI-2912): proves the SQL text — the websearch_to_tsquery
// grammar, the @@ match against the 0012 generated tsvector, the ts_rank_cd title>body
// ordering (0012 setweight), the ts_headline <mark> markup, and — the load-bearing part
// — the ADR-039 in-query RBAC scope (a Team-fenced query never returns another Team's
// rows; an admin AllTeams query returns every Team's). These are the pieces a fake
// searcher cannot prove.
//
// Same gate as internal/apiserver's audit integration test: CI provisions Postgres and runs
//
//	go test -tags=search_integration ./pkg/search/ -run TestSearchIntegration
//
// DATABASE_URL unset ⇒ SKIP (a developer without Postgres is not blocked).

package search

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

func openSearchTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping the search integration test (needs real Postgres)")
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
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// applySearchMigrations resets coord and applies the SHIPPED 0001 (work_item) + 0012
// (search_tsv + GIN) files — not inline DDL — so drift between a migration and the
// searcher goes RED here.
func applySearchMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Reset every schema the shipped migrations own so a re-run starts clean.
	for _, sch := range []string{"coord", "discussion", "auth"} {
		if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+sch+` CASCADE`); err != nil {
			t.Fatalf("reset %s schema: %v", sch, err)
		}
	}
	// Apply the FULL shipped migration chain in order — not a hand-picked subset — so the searcher runs
	// against the real work_item shape (search_tsv + the ADR-0024b source='board' board-hide column that
	// migration 0027 adds). Hand-picking 0001+0012 drifted RED once source was filtered in the SQL; the
	// whole chain keeps the lane honest as new columns land. Test-seed files (*_test.sql) are excluded.
	migDir := filepath.Join("..", "..", "db", "migrations")
	if _, err := os.Stat(migDir); err != nil {
		migDir = filepath.Join("db", "migrations")
	}
	files, err := filepath.Glob(filepath.Join(migDir, "0*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.sql") {
			continue // seed fixtures, not schema
		}
		names = append(names, f)
	}
	sort.Strings(names) // numeric filename prefix ⇒ lexical sort is apply order
	if len(names) == 0 {
		t.Fatalf("no shipped migrations found under %s", migDir)
	}
	for _, path := range names {
		mig, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", path, err)
		}
		if _, err := db.ExecContext(ctx, string(mig)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(path), err)
		}
	}
}

func seedItem(t *testing.T, db *sql.DB, projectID, teamID, title, body, state string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO coord.work_item (project_id, team_id, title, body, state, created_by)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, 'admin') RETURNING id::text`,
		projectID, teamID, title, body, state).Scan(&id)
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}
	return id
}

func TestSearchIntegration(t *testing.T) {
	db := openSearchTestDB(t)
	applySearchMigrations(t, db)
	s, err := NewPostgresSearcher(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	const teamA = "aaaaaaaa-0000-0000-0000-000000000001"
	const teamB = "bbbbbbbb-0000-0000-0000-000000000002"
	const projA = "aaaaaaaa-1111-0000-0000-000000000001"
	const projB = "bbbbbbbb-1111-0000-0000-000000000002"

	titleHit := seedItem(t, db, projA, teamA, "Fix checkout latency", "some unrelated body", "in_progress")
	_ = seedItem(t, db, projA, teamA, "Unrelated title", "the checkout flow is slow", "todo") // body hit
	bTeamHit := seedItem(t, db, projB, teamB, "Checkout on team B", "team B detail", "backlog")
	_ = seedItem(t, db, projA, teamA, "Billing rollup", "nothing relevant here", "done") // no hit

	// ── Team-fenced (non-admin): only team A's two checkout hits, NOT team B's ──
	got, err := s.Search(ctx, Query{Text: "checkout", TeamID: teamA, AllTeams: false, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("team-fenced: want 2 hits, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		if r.ID == bTeamHit {
			t.Fatal("team-fenced query leaked another Team's row (ADR-039 violation)")
		}
		if r.Type != "work_item" {
			t.Fatalf("unexpected type %q", r.Type)
		}
	}
	// Ranking: the title hit outranks the body-only hit (0012 setweight A>B).
	if got[0].ID != titleHit {
		t.Fatalf("expected the title hit ranked first, got %+v", got)
	}
	// Snippet carries the <mark> highlight and nothing but <mark> as a tag.
	if !strings.Contains(got[0].Snippet, "<mark>") || !strings.Contains(got[0].Snippet, "</mark>") {
		t.Fatalf("expected <mark> highlight in snippet, got %q", got[0].Snippet)
	}
	if got[0].ProjectID != projA {
		t.Fatalf("expected projectID %s, got %s", projA, got[0].ProjectID)
	}

	// ── Admin (AllTeams): every Team's checkout hits, including team B's ──
	all, err := s.Search(ctx, Query{Text: "checkout", AllTeams: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("admin fleet-wide: want 3 hits, got %d", len(all))
	}
	var sawB bool
	for _, r := range all {
		if r.ID == bTeamHit {
			sawB = true
		}
	}
	if !sawB {
		t.Fatal("admin query must include another Team's matching row")
	}

	// ── websearch grammar is forgiving: punctuation/negation never errors ──
	for _, q := range []string{`"checkout latency"`, `checkout -billing`, `checkout!!!`, `:::`} {
		if _, err := s.Search(ctx, Query{Text: q, TeamID: teamA, Limit: 5}); err != nil {
			t.Fatalf("query %q must not error, got %v", q, err)
		}
	}

	// ── An empty query is a caller error, not a DB round-trip ──
	if _, err := s.Search(ctx, Query{Text: "   ", TeamID: teamA}); err != ErrEmptyQuery {
		t.Fatalf("expected ErrEmptyQuery, got %v", err)
	}

	// ── A caller with no resolved Team (empty TeamID, non-admin) sees nothing ──
	none, err := s.Search(ctx, Query{Text: "checkout", TeamID: "", AllTeams: false, Limit: 20})
	if err != nil {
		t.Fatalf("empty-team query should fail closed to no rows, got err %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("empty-team (non-admin) query must return no rows, got %d", len(none))
	}
}

// TestSearchIntegration_PrefixGrammar proves the ISI-5213 ticket-picker grammar against real Postgres:
// a partial word typed while composing prefix-matches (a full-word websearch query would miss it), a
// stopword in the query no longer zeroes it, and a stopword-only query returns no rows WITHOUT erroring
// (so the picker shows an empty popover, never a 400). The default (websearch) grammar is proven above.
func TestSearchIntegration_PrefixGrammar(t *testing.T) {
	db := openSearchTestDB(t)
	applySearchMigrations(t, db)
	s, err := NewPostgresSearcher(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	const teamA = "aaaaaaaa-0000-0000-0000-000000000001"
	const projA = "aaaaaaaa-1111-0000-0000-000000000001"
	hit := seedItem(t, db, projA, teamA, "Fix the intake sweep", "unrelated body", "in_progress")

	// A partial word ("inta") is what a user has typed mid-word. websearch matches whole words only, so
	// the default grammar misses it; the prefix grammar matches "intake".
	if got, err := s.Search(ctx, Query{Text: "inta", TeamID: teamA, Limit: 10}); err != nil {
		t.Fatalf("default partial query errored: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("default (websearch) grammar should NOT prefix-match a partial word, got %d", len(got))
	}
	got, err := s.Search(ctx, Query{Text: "inta", TeamID: teamA, Limit: 10, Prefix: true})
	if err != nil {
		t.Fatalf("prefix partial query errored: %v", err)
	}
	if len(got) != 1 || got[0].ID != hit {
		t.Fatalf("prefix grammar must match the partial word to the intake ticket, got %+v", got)
	}

	// A stopword in the query ("the intake") no longer zeroes it — to_tsvector drops the stopword and the
	// remaining lexeme prefix-matches.
	if got, err := s.Search(ctx, Query{Text: "the intake", TeamID: teamA, Limit: 10, Prefix: true}); err != nil {
		t.Fatalf("prefix stopword query errored: %v", err)
	} else if len(got) != 1 || got[0].ID != hit {
		t.Fatalf("prefix grammar must tolerate a stopword and still match, got %+v", got)
	}

	// A stopword-only query lexizes to nothing: no rows, and crucially NOT ErrEmptyQuery (the picker
	// shows an empty popover instead of a 400 while the user is mid-stopword).
	if got, err := s.Search(ctx, Query{Text: "the", TeamID: teamA, Limit: 10, Prefix: true}); err != nil {
		t.Fatalf("prefix stopword-only query must not error, got %v", err)
	} else if len(got) != 0 {
		t.Fatalf("prefix stopword-only query must return no rows, got %d", len(got))
	}
}
