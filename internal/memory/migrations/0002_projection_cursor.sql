-- Story J-C (ISI-4010 / Epic 10 Story 10.2) — persistent projection cursor (durable watermark).
--
-- FORWARD-ONLY, additive. A small durable watermark so an out-of-band projection (the 10.2
-- discussion→memory indexer, §17.4) resumes from its last committed position across a restart instead
-- of a zero/in-memory default — no full re-scan and no skipped window (AC1). Paired with an idempotent
-- projection on the deterministic derived record id (ON CONFLICT in memory.memory_records), a re-scan of
-- the boundary window on restart is a no-op, so a crash between writing a batch and saving the cursor
-- drops and duplicates nothing (AC2).
--
-- This table NEVER gates a write and NEVER touches the trust model or the memory_records schema — it is
-- pure durability state for the best-effort relay. A missing/failing cursor stays fail-open: the sweep
-- simply falls back to the in-process watermark (AC3). Do NOT add a destructive statement on the forward
-- path (§7.4/NFR-REL3).

CREATE TABLE IF NOT EXISTS memory.projection_cursor (
    name              text        PRIMARY KEY,          -- projection id, e.g. "discussion"
    last_projected_at timestamptz NOT NULL,             -- max source created_at durably projected
    last_projected_id text        NOT NULL DEFAULT '',  -- the boundary row's source id (tie provenance)
    updated_at        timestamptz NOT NULL DEFAULT now()
);
