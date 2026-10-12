//go:build chaos

// party_advancer_adapter_integration_test.go — the real-Postgres gate for ISI-5680 (ISI-5662 WS3).
//
// The offline unit lane mocks the AdvancerWorkItemWriter, so it never exercises the one seam that
// actually broke in production: the party session's ProjectID is the room's "namespace/name" slug
// (discussion.party_session.project_id is TEXT — migration 0026), but coord.CreateWorkItem inserts
// project_id via $1::uuid. A slug reaching that cast errors ("invalid input syntax for type uuid"),
// the facilitator/takeaways mint aborts, and NO facilitator is dispatched — the exact live symptom on
// session c33ddc0a. These tests drive a real room slug through coordAdvancerWriter against the SHIPPED
// coord schema on a live Postgres, pinning both the fix (slug resolves → uuid insert succeeds) and the
// regression (a nil resolver lets the slug hit the cast and errors).
package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/mentiondispatch"
	"github.com/K8squad/K8squad/pkg/coord"
)

const (
	// advRoomSlug is the canonical room project reference — "namespace/name", exactly what a live
	// party_session.project_id carries (ISI-3982 / migration 0026). It is NOT a uuid, which is the whole
	// point: coord stores a Project UID.
	advRoomSlug = "bmad-squad/bmad-demo-project"
	// advProjectUID / advTeamUID are what a real ProjectResolver would map the slug to off the Project/Team
	// CRs. The coord.work_item.project_id / team_id columns are uuid, so these must be valid uuids.
	advProjectUID = "912e88e2-0000-4000-8000-000000000001"
	advTeamUID    = "eecf616a-0000-4000-8000-000000000002"
	advPrincipal  = "ksquad-operator"
)

// advFakeResolver maps the room slug to the resolved Project/Team UIDs, standing in for the
// client.Reader-backed mentiondispatch.NewClientProjectResolver the operator wires in production. A
// UID-bearing ref passes through unchanged (mirrors the real resolver's UID-first contract).
type advFakeResolver struct{ uid, teamUID string }

func (r advFakeResolver) ResolveProject(_ context.Context, ref string) (mentiondispatch.ResolvedProject, error) {
	if ref == advRoomSlug {
		return mentiondispatch.ResolvedProject{UID: r.uid, TeamUID: r.teamUID}, nil
	}
	// UID passthrough — a ref that is already a Project UID resolves to itself.
	return mentiondispatch.ResolvedProject{UID: ref}, nil
}

// advDSNOrFatal requires a live Postgres under -tags=chaos — a required DB gate must fail loud, never
// skip its own precondition (same contract as pkg/coord dsnOrFatal).
func advDSNOrFatal(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL unset under -tags=chaos: the ISI-5680 party-advancer gate requires a " +
			"live Postgres. Refusing to pass silently.")
	}
	return dsn
}

// advMigrationFile locates a shipped migration by name (COORD_MIGRATIONS_DIR override, else repo-root
// relative from ./cmd/operator).
func advMigrationFile(t *testing.T, name string) string {
	t.Helper()
	candidates := []string{}
	if dir := os.Getenv("COORD_MIGRATIONS_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	candidates = append(candidates,
		filepath.Join("..", "..", "db", "migrations", name),
		filepath.Join("db", "migrations", name),
	)
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Fatalf("ISI-5680 gate: cannot locate %s (looked in %v); set COORD_MIGRATIONS_DIR", name, candidates)
	return ""
}

// advResetCoordSchema re-applies the base spine (0001) + the create-time attribute columns (0020) into a
// clean coord schema. 0020 is mandatory: CreateWorkItem INSERTs/RETURNs work_item.priority/work_mode/labels.
func advResetCoordSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS coord CASCADE`,
		advMigrationFile(t, "0001_coord_schema.sql"),
		advMigrationFile(t, "0020_work_item_create_fields.sql"),
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ISI-5680 gate: apply coord schema: %v", err)
		}
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = db.ExecContext(cctx, `DROP SCHEMA IF EXISTS coord CASCADE`)
	})
}

func advOpenDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestPartyAdvancerWriter_ResolvesRoomSlugToUUIDProject is the fix: a room slug driven through
// coordAdvancerWriter (with the resolver the operator now wires) lands a coord.work_item whose uuid
// project_id is the RESOLVED UID — not the slug — so the facilitator mint succeeds instead of erroring
// on the $1::uuid cast.
func TestPartyAdvancerWriter_ResolvesRoomSlugToUUIDProject(t *testing.T) {
	ctx := context.Background()
	db := advOpenDB(t, advDSNOrFatal(t))
	advResetCoordSchema(t, ctx, db)

	store, err := coord.NewWorkItemWriteStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemWriteStore: %v", err)
	}
	w := coordAdvancerWriter{store: store, resolver: advFakeResolver{uid: advProjectUID, teamUID: advTeamUID}}

	res, err := w.CreateWorkItem(ctx, discussion.AdvancerCreateInput{
		ProjectID: advRoomSlug, // the slug — as the live advancer passes sess.ProjectID
		TeamID:    advTeamUID,  // sess.TeamID.String() is already a uuid
		Title:     "Party facilitation — round 1 of 3",
		Body:      "facilitate",
		Principal: advPrincipal,
	})
	if err != nil {
		t.Fatalf("CreateWorkItem with a room slug must succeed once resolved; got: %v", err)
	}
	if res.ID == "" {
		t.Fatal("expected a minted work item id")
	}

	// The row must key on the RESOLVED Project UID, not the slug (which could never have cast to uuid).
	var gotProject, gotTeam string
	if err := db.QueryRowContext(ctx,
		`SELECT project_id::text, team_id::text FROM coord.work_item WHERE id = $1::uuid`, res.ID,
	).Scan(&gotProject, &gotTeam); err != nil {
		t.Fatalf("read back minted work item: %v", err)
	}
	if gotProject != advProjectUID {
		t.Fatalf("work_item.project_id = %q, want resolved UID %q (slug must not reach the uuid column)", gotProject, advProjectUID)
	}
	if gotTeam != advTeamUID {
		t.Fatalf("work_item.team_id = %q, want %q", gotTeam, advTeamUID)
	}
}

// TestPartyAdvancerWriter_NilResolverSlugErrors pins the ISI-5680 regression: WITHOUT the resolver (the
// prior wiring), the slug reaches coord's $1::uuid cast and the INSERT errors — the silent mint failure
// that left sessions stalled at round 1 with no facilitator dispatched. This is the behaviour the fix
// removes; if a future change drops the resolver, this test goes red.
func TestPartyAdvancerWriter_NilResolverSlugErrors(t *testing.T) {
	ctx := context.Background()
	db := advOpenDB(t, advDSNOrFatal(t))
	advResetCoordSchema(t, ctx, db)

	store, err := coord.NewWorkItemWriteStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemWriteStore: %v", err)
	}
	w := coordAdvancerWriter{store: store} // nil resolver — slug passes through verbatim (the bug)

	_, err = w.CreateWorkItem(ctx, discussion.AdvancerCreateInput{
		ProjectID: advRoomSlug,
		TeamID:    advTeamUID,
		Title:     "Party facilitation — round 1 of 3",
		Body:      "facilitate",
		Principal: advPrincipal,
	})
	if err == nil {
		t.Fatal("a room slug driven through coord's uuid project_id column WITHOUT resolution must error " +
			"(invalid input syntax for type uuid) — the mint-failure ISI-5680 reported")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "uuid") {
		t.Fatalf("expected a uuid-cast error for the slug, got: %v", err)
	}
}

// advResetCoordSchemaWithSource re-applies the base spine (0001) + create-time attributes (0020) + the
// dispatch-on-mention source column (0027) into a clean coord schema. 0027's part (1) adds
// coord.work_item.source; its part (2) creates discussion.mention_dispatch, so the discussion schema must
// exist first. This is the schema the ISI-5692 source-flip test needs — the UPDATE markSource runs
// touches the source column 0027 introduces.
func advResetCoordSchemaWithSource(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS coord CASCADE`,
		`DROP SCHEMA IF EXISTS discussion CASCADE`,
		`CREATE SCHEMA discussion`,
		advMigrationFile(t, "0001_coord_schema.sql"),
		advMigrationFile(t, "0020_work_item_create_fields.sql"),
		advMigrationFile(t, "0027_dispatch_on_mention_run_minting.sql"),
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ISI-5692 gate: apply coord schema: %v", err)
		}
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = db.ExecContext(cctx, `DROP SCHEMA IF EXISTS coord CASCADE`)
		_, _ = db.ExecContext(cctx, `DROP SCHEMA IF EXISTS discussion CASCADE`)
	})
}

// TestPartyAdvancerWriter_FlipsSourceToDiscussion is the ISI-5692 fix: the facilitator/takeaways item the
// advancer mints must be stamped source='discussion' so its Run is assembled as a DISCUSSION run
// (discussion_post tool + CapabilityCoordinator token) and can post its round directive through the MCP
// edge ISI-5688 fixed. Without the markSource flip the item defaults to 'board' → an INTAKE run with no
// discussion endpoint → the round stalls with zero voices. This drives the real markSource UPDATE (the
// closure the operator wires) through the shipped schema and asserts the column landed 'discussion'.
func TestPartyAdvancerWriter_FlipsSourceToDiscussion(t *testing.T) {
	ctx := context.Background()
	db := advOpenDB(t, advDSNOrFatal(t))
	advResetCoordSchemaWithSource(t, ctx, db)

	store, err := coord.NewWorkItemWriteStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemWriteStore: %v", err)
	}
	w := coordAdvancerWriter{
		store:    store,
		resolver: advFakeResolver{uid: advProjectUID, teamUID: advTeamUID},
		markSource: func(ctx context.Context, workItemID string) error {
			_, err := db.ExecContext(ctx, `UPDATE coord.work_item SET source = 'discussion' WHERE id = $1::uuid`, workItemID)
			return err
		},
	}

	res, err := w.CreateWorkItem(ctx, discussion.AdvancerCreateInput{
		ProjectID: advRoomSlug,
		TeamID:    advTeamUID,
		Title:     "Party facilitation — round 1 of 3",
		Body:      "facilitate",
		Principal: advPrincipal,
	})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}

	var gotSource string
	if err := db.QueryRowContext(ctx,
		`SELECT source FROM coord.work_item WHERE id = $1::uuid`, res.ID,
	).Scan(&gotSource); err != nil {
		t.Fatalf("read back minted work item source: %v", err)
	}
	if gotSource != "discussion" {
		t.Fatalf("facilitator work_item.source = %q, want %q — an intake-sourced facilitator has no "+
			"discussion_post tool and stalls the round (ISI-5692)", gotSource, "discussion")
	}
}

// TestPartyAdvancerWriter_MarkSourceErrorAbortsMint pins the fail-loud contract: if the source flip fails,
// CreateWorkItem must return the error rather than hand back an intake-sourced facilitator that would
// silently reproduce the ISI-5692 zero-voice stall. The advancer logs and retries the mint next tick.
func TestPartyAdvancerWriter_MarkSourceErrorAbortsMint(t *testing.T) {
	ctx := context.Background()
	db := advOpenDB(t, advDSNOrFatal(t))
	advResetCoordSchemaWithSource(t, ctx, db)

	store, err := coord.NewWorkItemWriteStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemWriteStore: %v", err)
	}
	boom := errors.New("mark source boom")
	w := coordAdvancerWriter{
		store:      store,
		resolver:   advFakeResolver{uid: advProjectUID, teamUID: advTeamUID},
		markSource: func(context.Context, string) error { return boom },
	}

	_, err = w.CreateWorkItem(ctx, discussion.AdvancerCreateInput{
		ProjectID: advRoomSlug,
		TeamID:    advTeamUID,
		Title:     "Party facilitation — round 1 of 3",
		Body:      "facilitate",
		Principal: advPrincipal,
	})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("a markSource failure must abort the mint (wrapping the cause); got err=%v", err)
	}
}
