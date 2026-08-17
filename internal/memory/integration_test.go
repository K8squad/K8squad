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
