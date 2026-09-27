-- 0004_project_id_text.sql — ISI-4919: memory scope_project_id is the platform Project id, a
-- Kubernetes "namespace/name" slug (ISI-3982), NOT a uuid.
--
-- WHY: memory_records.project_id was uuid, matching the (now-corrected) discussion assumption that a
-- Project id is a uuid. In practice project narrowing was rarely populated, so the mismatch stayed
-- latent — until the discussion→memory index bridge (§17.4) began projecting real rooms, whose id is
-- the "namespace/name" slug. Inserting/filtering that slug against a uuid column fails with
-- `22P02 invalid input syntax for type uuid`, so discussion messages silently never entered recall for
-- any real Project. Retype to text to match the platform's canonical Project id, consistent with the
-- companion db/migrations/0026 on discussion.thread.project_id.
--
-- squad_id / principal_id / run_id / agent_id stay uuid — those ARE uuids. Only the Project scope is a
-- slug. The scope btree indexes (memory_records_scope, memory_records_scope_live) are rebuilt
-- automatically by the type change. Any legacy row that carried a uuid project_id keeps its value as
-- the uuid's text form (still squad-scoped; only project-narrowed recall by that old uuid stops
-- matching, which no live caller issues).
--
-- FORWARD-ONLY, additive-in-spirit (a widening retype of an optional scope column); applied once by the
-- memory migration runner (internal/memory/migrate.go) in lexical order, each in its own transaction.

ALTER TABLE memory.memory_records
    ALTER COLUMN project_id TYPE text USING project_id::text;
