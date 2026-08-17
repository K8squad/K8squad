// Package memory is the ksquad-memory service store: the §7.1/§7.2 knowledge-record substrate over
// Postgres + pgvector (arch §7, FR-E1, OQ10, ADR-004). This story (6.1 / ISI-2716) stands up the
// service + store; the MCP tool surface is 6.2, write-auth is 6.3, untrusted-read is 6.4, tenancy
// scoping is 6.5. The service integrates pgvector — it does NOT implement a bespoke vector engine.
package memory

import (
	"context"
	"encoding/json"
	"time"
)

// Record is a single knowledge record — the §7.2 memory_record shape. Scope + provenance are
// server-stamped by the write path (6.3 later *enforces* the auth; 6.1 provides the substrate).
type Record struct {
	ID            string
	SquadID       string  // §7.2 scope_team_id — tenancy root, NOT NULL
	ProjectID     *string // §7.2 scope_project_id — optional narrower scope
	PrincipalID   string  // §7.2 author_principal — NOT NULL
	RunID         *string // §7.2 author_run_id
	AgentID       *string // §7.2 author_agent_id
	Kind          string
	Content       string
	Embedding     []float32
	CreatedAt     time.Time
	InvalidatedAt *time.Time
	Provenance    json.RawMessage
}

// WriteRequest is a memory.write. id and created_at are server-stamped (never client-supplied);
// the embedding dimension must match the configured embedder (checked at write time, not silently).
type WriteRequest struct {
	SquadID     string
	ProjectID   *string
	PrincipalID string
	RunID       *string
	AgentID     *string
	Kind        string
	Content     string
	Embedding   []float32
	Provenance  json.RawMessage
}

// SearchQuery is a scoped semantic search. It is ALWAYS scoped by SquadID (the tenancy root, AC3);
// the enforcement predicate against the caller's principal is 6.5 — here scope is a required input.
type SearchQuery struct {
	SquadID   string
	Embedding []float32
	Limit     int
}

// SearchHit is a ranked result — the record plus its pgvector cosine distance to the query.
type SearchHit struct {
	Record
	Distance float64
}

// MemoryBackend is the §7.1/§7.6 storage/retrieval seam. pgvector is the default and only v1 backend
// (AC5); the seam exists so GRAIL/alt backends can plug in later without touching the service. It is
// NOT a place to hide an app-side vector scan — every implementation MUST push semantic search into
// its store (OQ10/ADR-004).
type MemoryBackend interface {
	// Ready reports the store is usable: pgvector present, schema applied at the expected version.
	// It fails closed — an absent extension or unexpected schema version is an error, never a
	// silent fallback to a bespoke store (AC1).
	Ready(ctx context.Context) error

	// Write stamps and durably commits a knowledge record, returning its server-assigned id.
	Write(ctx context.Context, req WriteRequest) (Record, error)

	// Search runs a scoped ANN semantic search pushed into the backend (AC3). Results are ordered by
	// distance ascending, exclude soft-retracted rows, and are scoped to q.SquadID.
	Search(ctx context.Context, q SearchQuery) ([]SearchHit, error)

	// Invalidate is the §7.4 soft-retract: stamp invalidated_at, never DELETE. Returns whether a live
	// record was retracted.
	Invalidate(ctx context.Context, id string) (bool, error)

	// Close releases the backend's resources.
	Close()
}
