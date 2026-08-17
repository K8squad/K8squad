package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgVectorStore is the v1 Backend: it integrates pgvector as the source-of-truth store (AC5).
// Semantic search is pushed into Postgres via the `<=>` cosine operator over the hnsw ANN index — the
// service never pulls rows and cosines them in-process (OQ10/ADR-004). Compile-time seam check below.
type PgVectorStore struct {
	pool *pgxpool.Pool
	dim  int
}

var _ Backend = (*PgVectorStore)(nil)

// Open connects to Postgres, applies the embedded migrations, and fails closed unless pgvector is
// present and the schema is at the expected version (AC1). It never degrades to an app-side store.
func Open(ctx context.Context, cfg Config) (*PgVectorStore, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := applyMigrations(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	s := &PgVectorStore{pool: pool, dim: EmbeddingDim}
	if err := s.Ready(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Ready fails closed if pgvector is absent or the base migration is not applied — the AC1 readiness
// gate. A missing extension is a hard error, not a silent fallback to a bespoke store.
func (s *PgVectorStore) Ready(ctx context.Context) error {
	var hasVector bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&hasVector); err != nil {
		return fmt.Errorf("probe pgvector extension: %w", err)
	}
	if !hasVector {
		return fmt.Errorf("pgvector extension absent — refusing to start (integrate pgvector, do not invent a vector engine; OQ10/ADR-004)")
	}
	var hasBase bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM memory.schema_migrations WHERE version = 'migrations/0001_memory.sql')`).Scan(&hasBase); err != nil {
		return fmt.Errorf("probe schema version: %w", err)
	}
	if !hasBase {
		return fmt.Errorf("memory schema not at expected version (0001 not applied) — refusing to start")
	}
	return nil
}

// Write server-stamps id + created_at (DB defaults) and durably commits the record. The embedding
// dimension is validated against the configured embedder so a mismatch is a legible error, not a
// silent write (§7.1). Scope/provenance NOT-NULL discipline is enforced by the schema itself.
func (s *PgVectorStore) Write(ctx context.Context, req WriteRequest) (Record, error) {
	if req.SquadID == "" {
		return Record{}, fmt.Errorf("squad_id is required (tenancy root, §7.3)")
	}
	if req.PrincipalID == "" {
		return Record{}, fmt.Errorf("principal_id is required (author, §7.3)")
	}
	if req.Kind == "" || req.Content == "" {
		return Record{}, fmt.Errorf("kind and content are required")
	}
	if len(req.Embedding) != s.dim {
		return Record{}, fmt.Errorf("embedding dimension %d != configured embedder dimension %d", len(req.Embedding), s.dim)
	}
	prov := req.Provenance
	if len(prov) == 0 {
		prov = json.RawMessage(`{}`)
	}

	const q = `
		INSERT INTO memory.memory_records
			(squad_id, project_id, principal_id, run_id, agent_id, kind, content, embedding, provenance)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::vector, $9)
		RETURNING id, created_at`
	rec := Record{
		SquadID:     req.SquadID,
		ProjectID:   req.ProjectID,
		PrincipalID: req.PrincipalID,
		RunID:       req.RunID,
		AgentID:     req.AgentID,
		Kind:        req.Kind,
		Content:     req.Content,
		Embedding:   req.Embedding,
		Provenance:  prov,
	}
	if err := s.pool.QueryRow(ctx, q,
		req.SquadID, req.ProjectID, req.PrincipalID, req.RunID, req.AgentID,
		req.Kind, req.Content, encodeVector(req.Embedding), []byte(prov),
	).Scan(&rec.ID, &rec.CreatedAt); err != nil {
		return Record{}, fmt.Errorf("insert memory_record: %w", err)
	}
	return rec, nil
}

// Search runs the AC3 semantic search: an ANN order-by the pgvector cosine operator, scoped by
// squad_id and filtered on invalidated_at IS NULL — pushed entirely into Postgres. The query embedding
// is bound as a `vector` parameter; the distance is computed by pgvector, never in this process.
func (s *PgVectorStore) Search(ctx context.Context, query SearchQuery) ([]SearchHit, error) {
	if query.SquadID == "" {
		return nil, fmt.Errorf("squad_id is required — semantic search is scoped to the tenancy root (AC3)")
	}
	if len(query.Embedding) != s.dim {
		return nil, fmt.Errorf("query embedding dimension %d != configured embedder dimension %d", len(query.Embedding), s.dim)
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 10
	}

	// $2 is bound as a text pgvector literal and cast `::vector`; the `<=>` cosine distance is then
	// computed by pgvector over the hnsw index. Both references cast the same param.
	const q = `
		SELECT id, squad_id, project_id, principal_id, run_id, agent_id, kind, content,
		       created_at, invalidated_at, provenance,
		       embedding <=> $2::vector AS distance
		FROM memory.memory_records
		WHERE squad_id = $1 AND invalidated_at IS NULL
		ORDER BY embedding <=> $2::vector
		LIMIT $3`
	rows, err := s.pool.Query(ctx, q, query.SquadID, encodeVector(query.Embedding), limit)
	if err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var prov []byte
		if err := rows.Scan(
			&h.ID, &h.SquadID, &h.ProjectID, &h.PrincipalID, &h.RunID, &h.AgentID,
			&h.Kind, &h.Content, &h.CreatedAt, &h.InvalidatedAt, &prov, &h.Distance,
		); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		h.Provenance = json.RawMessage(prov)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search hits: %w", err)
	}
	return hits, nil
}

// Invalidate is the §7.4 soft-retract: it stamps invalidated_at on a live record, never DELETEs it.
func (s *PgVectorStore) Invalidate(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE memory.memory_records SET invalidated_at = now()
		 WHERE id = $1 AND invalidated_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("invalidate memory_record: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Close releases the connection pool.
func (s *PgVectorStore) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Pool exposes the underlying pool for integration tests and future co-located schemas (discussion,
// §7.5). Not part of the Backend seam.
func (s *PgVectorStore) Pool() *pgxpool.Pool { return s.pool }

// encodeVector renders a float32 slice as a pgvector text literal ("[0.1,0.2,...]"). Bound as a text
// parameter and cast `::vector` in SQL, this keeps the wire representation exact and avoids an extra
// dependency — the distance math still runs in pgvector, not here.
func encodeVector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*8 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
