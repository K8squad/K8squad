-- Story 6.1 (ISI-2716 / ISI-2222) — ksquad-memory knowledge-record substrate.
-- Arch §7.1/§7.2/§7.3/§7.4, FR-E1, OQ10, ADR-004 (integrate pgvector, do not invent a vector engine).
--
-- FORWARD-ONLY. Memory writes are durable Postgres commits; retraction is the soft `invalidated_at`
-- column, never a destructive DROP/DELETE/TRUNCATE of the record (§7.4/NFR-REL3). Do NOT add a
-- destructive statement to this file on the forward path.

-- Integrate the store (ADR-004/OQ10): pgvector is the v1 source-of-truth backend. Fail closed at the
-- service if this extension cannot be created — never silently degrade to an app-side scan.
CREATE EXTENSION IF NOT EXISTS vector;

-- Dedicated `memory` schema (§17.3: one Postgres, many schemas — coord/memory/discussion/…).
CREATE SCHEMA IF NOT EXISTS memory;

-- The knowledge record. Column set is FR-E1 / arch §7.2; the issue's public spelling maps 1:1 to the
-- §7.2 authoritative names (squad_id=scope_team_id, principal_id=author_principal, created_at=written_at).
-- The load-bearing scope/provenance columns are NOT NULL so an unscoped/unattributed row is not even
-- representable (§7.3 substrate; the write-auth *check* is 6.3, the tenancy *predicate* is 6.5).
--
-- embedding is a real pgvector `vector(dim)` — NOT bytea/json/float[]-in-text (INV1/INV2, the OQ10 crux).
-- dim 768 = the default local embedder (ksquad-system, §7.1); the embedder id/model+dim is recorded in
-- `provenance`, so a dimension mismatch is detectable at write time, not silent.
CREATE TABLE IF NOT EXISTS memory.memory_records (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    squad_id       uuid        NOT NULL,                 -- §7.2 scope_team_id — tenancy root (6.5 keys here)
    project_id     uuid,                                 -- §7.2 scope_project_id — optional narrower scope
    principal_id   uuid        NOT NULL,                 -- §7.2 author_principal — write-auth substrate (6.3)
    run_id         uuid,                                 -- §7.2 author_run_id — provenance
    agent_id       uuid,                                 -- §7.2 author_agent_id — provenance
    kind           text        NOT NULL,                 -- fact / decision / note / handoff-mirror
    content        text        NOT NULL,                 -- the knowledge text
    embedding      vector(768) NOT NULL,                 -- pgvector type — the INV1/INV2 crux
    created_at     timestamptz NOT NULL DEFAULT now(),   -- §7.2 written_at (server-stamped)
    invalidated_at timestamptz,                          -- §7.4 soft-retract — supersede without destroying audit
    provenance     jsonb       NOT NULL DEFAULT '{}'::jsonb -- extensible envelope (tags/trust/embedder id+model+dim)
);

-- Integrate the store: an ANN index on the pgvector column so semantic search is pushed into Postgres
-- (ORDER BY embedding <=> $q), plus a btree scope index for the §7.3.3 / 6.5 tenancy filter.
CREATE INDEX IF NOT EXISTS memory_records_embedding_ann
    ON memory.memory_records USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS memory_records_scope
    ON memory.memory_records (squad_id);
-- Live-record scope filter accelerator: the default search plan filters `invalidated_at IS NULL`.
CREATE INDEX IF NOT EXISTS memory_records_scope_live
    ON memory.memory_records (squad_id)
    WHERE invalidated_at IS NULL;
