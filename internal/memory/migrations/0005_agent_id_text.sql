-- 0005_agent_id_text.sql — ISI-5210 (from ISI-5209): memory author_agent_id is an agent NAME, not a uuid.
--
-- WHY: memory_records.agent_id was uuid (0001), and 0004 explicitly asserted "agent_id ARE uuids". That
-- assumption is wrong. The whole authoring/token identity model keys an agent by its stable NAME
-- (`run.Spec.Agents[0].Name`), which is exactly what the operator stamps into the run token's AgentID
-- claim and the memory edge forwards as X-Agent-Id (pkg/controller/run/assembly.go). Principal and Run
-- ARE uuids (run.GetOwnedBy() / run.UID) — only the agent identity is a name. So:
--   - diary_append / memory_write stamp agent_id = <name> into a uuid column → `22P02 invalid input
--     syntax for type uuid` on the INSERT, and
--   - diary_read(agent) casts `agent_id = $2::uuid` against a name arg the model passes (e.g. "john") →
--     the same 22P02, surfaced as an empty/errored tool chip (run intake-0b4d733e).
-- Retype to text to match the platform's canonical agent identifier — consistent with the discussion
-- substrate, whose author agent_id column is already text (internal/discussion/store.go), and with the
-- provenance envelope that has always carried the honest agent NAME (§7.3.2).
--
-- squad_id / principal_id / run_id stay uuid — those ARE uuids (tenancy root, author principal, run UID).
-- Only the agent identity is a name. The partial diary index (memory_records_diary_chrono, 0003) and the
-- scope indexes are rebuilt automatically by the type change. Any legacy row that carried a derived-uuid
-- agent_id (the discussion→memory projection's deriveUUID) keeps its value as the uuid's text form; those
-- rows are kind='discussion' and are never diary-read, so no live read changes behaviour.
--
-- FORWARD-ONLY, additive-in-spirit (a widening retype of an optional provenance column); applied once by
-- the memory migration runner (internal/memory/migrate.go) in lexical order, each in its own transaction.
-- The companion binary change retypes expectedColumnTypes["agent_id"]="text" and the ReadChronological
-- ::uuid cast to ::text in the SAME release, so the ISI-5112 deploy-skew guard stays honest.

ALTER TABLE memory.memory_records
    ALTER COLUMN agent_id TYPE text USING agent_id::text;
