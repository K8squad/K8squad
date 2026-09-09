-- Story 6.2 diary ergonomics (ISI-4077, follow-up to ISI-3179) — chronological diary read accelerator.
-- Arch §8.3 MVP tool surface: diary_read(agent, last_n) reads an agent's last N diary entries in time
-- order. That read is `WHERE squad_id = $1 AND agent_id = $2 AND kind = 'diary' AND invalidated_at IS NULL
-- ORDER BY created_at DESC LIMIT n` (internal/memory/store.go ReadChronological) — a path the existing
-- ANN (embedding <=>) and scope-only indexes do not serve. A partial btree keyed on the scope + owner and
-- ordered by created_at DESC turns it into an index-only range scan (no sort, no embedding).
--
-- FORWARD-ONLY (matches 0001): no destructive statement on the forward path. CREATE INDEX IF NOT EXISTS is
-- idempotent; a plain (non-CONCURRENT) build so it commits inside the per-file migration transaction.

CREATE INDEX IF NOT EXISTS memory_records_diary_chrono
    ON memory.memory_records (squad_id, agent_id, created_at DESC)
    WHERE kind = 'diary' AND invalidated_at IS NULL;
