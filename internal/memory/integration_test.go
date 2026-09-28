//go:build integration

// Real-pgvector integration test for Story 6.1 (AC1/AC3/AC4). It runs against a live Postgres+pgvector
// (schema exists on start via the applied migration) and exercises the round trip: write embeddings,
// run the `<=>` ANN semantic search, assert results come back scoped and ranked, and assert
// soft-retract removes a row from the read path. Analogous to Story 2.7's real-PG arm for coord.
//
//	MEMORY_TEST_DATABASE_URL=postgres://postgres:password@localhost:5432/ksquad?sslmode=disable \
//	  go test -tags integration ./internal/memory/...
//
// Requires a pgvector-enabled image (e.g. pgvector/pgvector:pg16). The test creates the extension via
// the migration; the connecting role must be allowed to CREATE EXTENSION.
package memory

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T) *PgVectorStore {
	t.Helper()
	dsn := os.Getenv("MEMORY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MEMORY_TEST_DATABASE_URL to run the pgvector integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, Config{DatabaseURL: dsn, EmbedderModel: "test"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// oneHot builds a unit-ish EmbeddingDim vector with a spike at index i, so cosine distance orders by
// which spike a query aligns with — deterministic nearest-neighbour without a real embedder.
func oneHot(i int) []float32 {
	v := make([]float32, EmbeddingDim)
	v[i%EmbeddingDim] = 1
	v[(i+1)%EmbeddingDim] = 0.1
	return v
}

func TestPgVector_WriteSearchRoundTrip(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	squad := uuid.NewString()
	principal := uuid.NewString()

	near, err := store.Write(ctx, WriteRequest{
		SquadID: squad, PrincipalID: principal, Kind: "fact",
		Content: "the near fact", Embedding: oneHot(3),
	})
	if err != nil {
		t.Fatalf("write near: %v", err)
	}
	if _, err := store.Write(ctx, WriteRequest{
		SquadID: squad, PrincipalID: principal, Kind: "fact",
		Content: "the far fact", Embedding: oneHot(500),
	}); err != nil {
		t.Fatalf("write far: %v", err)
	}

	hits, err := store.Search(ctx, SearchQuery{SquadID: squad, Embedding: oneHot(3), Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	if hits[0].Content != "the near fact" {
		t.Fatalf("nearest hit = %q, want %q", hits[0].Content, "the near fact")
	}
	if hits[0].ID != near.ID {
		t.Fatalf("nearest id = %s, want %s", hits[0].ID, near.ID)
	}
	// AC3: distance is computed by pgvector and returned ascending.
	for i := 1; i < len(hits); i++ {
		if hits[i].Distance < hits[i-1].Distance {
			t.Fatalf("hits not ordered by distance: %v", hits)
		}
	}
}

func TestPgVector_ScopedBySquad(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	squadA, squadB := uuid.NewString(), uuid.NewString()
	principal := uuid.NewString()

	if _, err := store.Write(ctx, WriteRequest{
		SquadID: squadA, PrincipalID: principal, Kind: "fact",
		Content: "squad A secret", Embedding: oneHot(7),
	}); err != nil {
		t.Fatalf("write A: %v", err)
	}
	// Searching squad B must never see squad A's record (AC3 scope by squad_id).
	hits, err := store.Search(ctx, SearchQuery{SquadID: squadB, Embedding: oneHot(7), Limit: 10})
	if err != nil {
		t.Fatalf("search B: %v", err)
	}
	for _, h := range hits {
		if h.Content == "squad A secret" {
			t.Fatalf("cross-squad leak: squad B search returned squad A record")
		}
	}
}

func TestPgVector_SoftRetractHidesFromSearch(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	squad := uuid.NewString()
	principal := uuid.NewString()

	rec, err := store.Write(ctx, WriteRequest{
		SquadID: squad, PrincipalID: principal, Kind: "fact",
		Content: "retractable fact", Embedding: oneHot(11),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	ok, err := store.Invalidate(ctx, rec.ID)
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if !ok {
		t.Fatal("expected invalidate to retract a live record")
	}

	// AC4: soft-retracted rows must not resurface on the read path.
	hits, err := store.Search(ctx, SearchQuery{SquadID: squad, Embedding: oneHot(11), Limit: 10})
	if err != nil {
		t.Fatalf("search after retract: %v", err)
	}
	for _, h := range hits {
		if h.ID == rec.ID {
			t.Fatalf("soft-retracted record %s resurfaced in search", rec.ID)
		}
	}

	// Re-invalidating an already-retracted record is a no-op (idempotent, non-destructive).
	if ok, _ := store.Invalidate(ctx, rec.ID); ok {
		t.Fatal("second invalidate should report no live record retracted")
	}
}

func TestPgVector_RejectsDimMismatch(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, err := store.Write(ctx, WriteRequest{
		SquadID: uuid.NewString(), PrincipalID: uuid.NewString(), Kind: "fact",
		Content: "bad dim", Embedding: []float32{1, 2, 3},
	})
	if err == nil {
		t.Fatal("expected dimension-mismatch error, got nil")
	}
}

// TestPgVector_SupersedeHandoffMirrors is the Story 6.6 republish discipline against real PG: a
// REPUBLISHED handoff (new audit row ⇒ new mirror write) soft-retracts every EARLIER live
// handoff-mirror of the same (squad, work item, run) except the just-written one, so scoped recall
// surfaces exactly the newest publication — and the superseded mirrors stay in the table (audit),
// never DELETEd.
func TestPgVector_SupersedeHandoffMirrors(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	squad, principal := uuid.NewString(), uuid.NewString()
	wi, run := uuid.NewString(), uuid.NewString()
	writeMirror := func(content string, embedding []float32) Record {
		t.Helper()
		runID := deriveUUIDForTest("run", run)
		rec, err := store.Write(ctx, WriteRequest{
			SquadID: squad, PrincipalID: principal, RunID: &runID,
			Kind: KindHandoffMirror, Content: content, Embedding: embedding,
			Provenance: NewHandoffProvenance("coord+audit://1", "sha", wi, run, 1, nil, "agent-a", nil, time.Now()),
		})
		if err != nil {
			t.Fatalf("write mirror: %v", err)
		}
		return rec
	}

	old1 := writeMirror(`{"did":["first"]}`, oneHot(21))
	old2 := writeMirror(`{"did":["second"]}`, oneHot(22))
	// A DIFFERENT run's mirror on the same work item — must NEVER be retracted by this pair's supersede.
	otherRec, err := store.Write(ctx, WriteRequest{
		SquadID: squad, PrincipalID: principal,
		Kind: KindHandoffMirror, Content: `{"did":["different run"]}`, Embedding: oneHot(24),
		Provenance: NewHandoffProvenance("coord+audit://2", "sha2", wi, uuid.NewString(), 2, nil, "agent-a", nil, time.Now()),
	})
	if err != nil {
		t.Fatalf("write other-run mirror: %v", err)
	}

	newest := writeMirror(`{"did":["republished"]}`, oneHot(25))
	n, err := store.SupersedeHandoffMirrors(ctx, squad, wi, run, newest.ID)
	if err != nil {
		t.Fatalf("SupersedeHandoffMirrors: %v", err)
	}
	if n != 2 {
		t.Fatalf("superseded %d, want 2 (old1 + old2 of the same pair)", n)
	}

	// Scoped recall over the pair's squad surfaces ONLY the newest publication.
	hits, err := store.Search(ctx, SearchQuery{
		SquadID: squad, Kind: strPtr(KindHandoffMirror), Embedding: oneHot(21), Limit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	live := map[string]bool{}
	for _, h := range hits {
		live[h.ID] = true
	}
	if live[old1.ID] || live[old2.ID] {
		t.Fatal("stale mirrors of the republished pair still live in search")
	}
	if !live[newest.ID] {
		t.Fatal("newest mirror missing from search")
	}
	if !live[otherRec.ID] {
		t.Fatal("a DIFFERENT run's mirror must never be retracted by this pair's supersede")
	}

	// Idempotent: re-running with the same keepID retracts nothing more.
	if n, _ := store.SupersedeHandoffMirrors(ctx, squad, wi, run, newest.ID); n != 0 {
		t.Fatalf("re-supersede retracted %d, want 0 (idempotent)", n)
	}
}

// TestPgVector_ReadChronological is the ISI-4077 diary read path against real PG: an agent's diary rows
// come back newest-first (created_at DESC), scoped to the caller squad + that agent + kind=diary, bounded
// by the limit, and soft-retracted rows never surface. Cross-squad, cross-agent, and non-diary rows are
// invisible to the read — the same scope/retraction discipline as Search, on the chronological ordering.
func TestPgVector_ReadChronological(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	squad, principal := uuid.NewString(), uuid.NewString()
	agent := uuid.NewString()
	otherAgent := uuid.NewString()

	// Write three diary rows for `agent` with strictly increasing created_at (explicit sleeps so the
	// server-stamped now() ordering is unambiguous), plus decoys the read must exclude.
	writeDiary := func(who, content string, emb []float32) Record {
		t.Helper()
		a := who
		rec, err := store.Write(ctx, WriteRequest{
			SquadID: squad, PrincipalID: principal, AgentID: &a,
			Kind: KindDiary, Content: content, Embedding: emb,
		})
		if err != nil {
			t.Fatalf("write diary: %v", err)
		}
		return rec
	}

	writeDiary(agent, "entry one", oneHot(30))
	time.Sleep(5 * time.Millisecond)
	writeDiary(agent, "entry two", oneHot(31))
	time.Sleep(5 * time.Millisecond)
	newest := writeDiary(agent, "entry three", oneHot(32))

	// Decoys: another agent's diary, and a non-diary row for the same agent.
	writeDiary(otherAgent, "other agent diary", oneHot(33))
	if _, err := store.Write(ctx, WriteRequest{
		SquadID: squad, PrincipalID: principal, AgentID: &agent,
		Kind: KindNote, Content: "a note, not a diary entry", Embedding: oneHot(34),
	}); err != nil {
		t.Fatalf("write note decoy: %v", err)
	}

	hits, err := store.ReadChronological(ctx, squad, agent, KindDiary, 10)
	if err != nil {
		t.Fatalf("ReadChronological: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("want 3 diary rows for the agent, got %d", len(hits))
	}
	// Newest first.
	if hits[0].Content != "entry three" || hits[2].Content != "entry one" {
		t.Fatalf("chronological order wrong: got %q..%q, want newest 'entry three' first", hits[0].Content, hits[2].Content)
	}
	for _, h := range hits {
		if h.Kind != KindDiary {
			t.Fatalf("non-diary row leaked: kind=%q", h.Kind)
		}
		if h.AgentID == nil || *h.AgentID != agent {
			t.Fatalf("cross-agent row leaked: agent=%v", h.AgentID)
		}
	}

	// The limit bounds the read (newest N).
	limited, err := store.ReadChronological(ctx, squad, agent, KindDiary, 2)
	if err != nil {
		t.Fatalf("ReadChronological limited: %v", err)
	}
	if len(limited) != 2 || limited[0].Content != "entry three" {
		t.Fatalf("limit=2 should return the 2 newest, got %d starting %q", len(limited), func() string {
			if len(limited) > 0 {
				return limited[0].Content
			}
			return ""
		}())
	}

	// Soft-retract removes a diary row from the chronological read (AC4 discipline on this path too).
	if ok, err := store.Invalidate(ctx, newest.ID); err != nil || !ok {
		t.Fatalf("invalidate newest: ok=%v err=%v", ok, err)
	}
	afterRetract, err := store.ReadChronological(ctx, squad, agent, KindDiary, 10)
	if err != nil {
		t.Fatalf("ReadChronological after retract: %v", err)
	}
	for _, h := range afterRetract {
		if h.ID == newest.ID {
			t.Fatalf("soft-retracted diary row %s resurfaced", newest.ID)
		}
	}
	if len(afterRetract) != 2 {
		t.Fatalf("want 2 live diary rows after retract, got %d", len(afterRetract))
	}

	// Cross-squad isolation: another squad sees none of this agent's diary.
	if cross, err := store.ReadChronological(ctx, uuid.NewString(), agent, KindDiary, 10); err != nil {
		t.Fatalf("cross-squad read: %v", err)
	} else if len(cross) != 0 {
		t.Fatalf("cross-squad diary leak: got %d rows", len(cross))
	}
}

// deriveUUIDForTest mirrors the bridges' deterministic text→uuid derivation so integration rows look
// exactly like the handoffmirror package's writes.
func deriveUUIDForTest(prefix, text string) string {
	return uuid.NewSHA1(uuid.MustParse("6b1e5b1e-2c9a-5e7d-9f3a-10b2c3d4e5f6"), []byte(prefix+":"+text)).String()
}

func strPtr(s string) *string { return &s }

// TestPgVector_SquadIDScopeUnderSimpleProtocol is the ISI-5114 regression (Defect A of ISI-4540). It
// STRUCTURALLY executes every squad_id-scoped read arm against real pgvector under the SIMPLE query
// protocol — the exec mode the production pool resolves to behind a transaction-pooling proxy, where
// pgx interpolates a Go string argument as a `$n::text` literal instead of leaning on the server-side
// parameter-type inference the default cache_statement mode enjoys. Under that protocol an uncast
// `WHERE squad_id = $1` predicate resolves to `text = uuid` and dies with SQLSTATE 42883 ("operator
// does not exist: text = uuid") on 100% of runs, before a single row is scanned — exactly the failure
// that zeroed out the dispatch pipeline. squad_id / principal_id / run_id / agent_id are uuid columns
// (migration 0004 kept them uuid; only project_id became text), so the fix is a `::uuid` cast on every
// squad_id predicate, matching the diary arm.
//
// This is deliberately NOT a sqlmock regex assertion: a regex matcher passes a uuid/text mismatch
// silently (the 42883 / 42P18 lesson). Only a real engine in the reproducing protocol catches a
// regressed cast — so if any squad_id predicate loses its `::uuid`, this test fails the build.
func TestPgVector_SquadIDScopeUnderSimpleProtocol(t *testing.T) {
	dsn := os.Getenv("MEMORY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MEMORY_TEST_DATABASE_URL to run the pgvector integration test")
	}
	ctx := context.Background()

	// Apply migrations / assert readiness once via the normal store (default exec mode).
	_ = testStore(t)

	// A second pool pinned to the simple protocol — this is the mode that reproduces the 42883 that the
	// default extended-protocol pool masks via server-side parameter type inference.
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool (simple protocol): %v", err)
	}
	t.Cleanup(pool.Close)
	store := &PgVectorStore{pool: pool, dim: EmbeddingDim}

	squad, principal := uuid.NewString(), uuid.NewString()
	agent := uuid.NewString()

	// Write itself uses squad_id in INSERT assignment context (text→uuid assignment cast is legal), so it
	// is not the defect — but we need a row to read back, and this proves the write path too.
	rec, err := store.Write(ctx, WriteRequest{
		SquadID: squad, PrincipalID: principal, AgentID: &agent,
		Kind: KindDiary, Content: "simple-protocol scope row", Embedding: oneHot(41),
	})
	if err != nil {
		t.Fatalf("write under simple protocol: %v", err)
	}

	// Search (store.go L180) — the proven ISI-4540 blocker. Uncast squad_id ⇒ 42883 here.
	if _, err := store.Search(ctx, SearchQuery{SquadID: squad, Embedding: oneHot(41), Limit: 5}); err != nil {
		t.Fatalf("Search under simple protocol (squad_id ::uuid cast regressed?): %v", err)
	}
	// SearchByIDs (store.go L238) — the sibling pinned-read arm.
	if _, err := store.SearchByIDs(ctx, SearchQuery{SquadID: squad}, []string{rec.ID}); err != nil {
		t.Fatalf("SearchByIDs under simple protocol (squad_id ::uuid cast regressed?): %v", err)
	}
	// ReadChronological (store.go L297) — the diary arm ISI-5110/5109 already cast; guarded here so it
	// can never regress silently.
	if _, err := store.ReadChronological(ctx, squad, agent, KindDiary, 10); err != nil {
		t.Fatalf("ReadChronological under simple protocol (squad_id ::uuid cast regressed?): %v", err)
	}
	// SupersedeHandoffMirrors (store.go L352) — the sibling writer-side arm (UPDATE ... WHERE squad_id).
	if _, err := store.SupersedeHandoffMirrors(ctx, squad, uuid.NewString(), uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatalf("SupersedeHandoffMirrors under simple protocol (squad_id ::uuid cast regressed?): %v", err)
	}
}
