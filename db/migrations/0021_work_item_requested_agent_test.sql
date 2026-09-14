-- 0021_work_item_requested_agent_test.sql — runnable self-check for the durable
-- pre-run agent-intent column (ADR-0022 §3 D2, ISI-4411).
--
-- Same discipline as the 0019 self-check: plain SQL, no framework, run after the
-- migrations it checks, inside one transaction ROLLED BACK at the end so it
-- leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0021_work_item_requested_agent.sql \
--          -f db/migrations/0021_work_item_requested_agent_test.sql
--
-- This file proves the STRUCTURAL ACs of the schema (column exists, is nullable,
-- persists a value, and the dispatch write shape: the conditional backlog→todo
-- advance that stamps requested_agent + the append-only audit row). The Go
-- writer's behaviour (agent-∈-Team enforcement, tenancy 404, idempotency 409) is
-- exercised by pkg/coord + internal/apiserver tests.

BEGIN;

-- (1) The column exists, is text, and is NULLABLE (no backfill / no default).
DO $$
DECLARE nullable text; dtype text;
BEGIN
    SELECT is_nullable, data_type INTO nullable, dtype
      FROM information_schema.columns
     WHERE table_schema = 'coord' AND table_name = 'work_item'
       AND column_name = 'requested_agent';
    ASSERT nullable = 'YES', 'requested_agent must be nullable (a fresh item carries no agent choice)';
    ASSERT dtype = 'text', 'requested_agent must be text (an agent name ref, not an FK)';
END $$;

-- (2) A fresh item defaults requested_agent to NULL (Intake → Team.Spec.Agents[0]).
DO $$
DECLARE wi uuid; ra text;
BEGIN
    INSERT INTO coord.work_item (project_id, team_id, title, created_by)
         VALUES (gen_random_uuid(), gen_random_uuid(), 'dispatch item', 'user:alice')
      RETURNING id INTO wi;
    SELECT requested_agent INTO ra FROM coord.work_item WHERE id = wi;
    ASSERT ra IS NULL, 'a create must leave requested_agent NULL (no backfill)';
END $$;

-- (3) The dispatch write shape: the conditional backlog→todo advance that stamps
--     requested_agent lands exactly once on a backlog item (the store's CAS), and
--     a re-dispatch of the now-todo item is a no-op (0 rows → the store's 409).
DO $$
DECLARE wi uuid; n int; st text; ra text;
BEGIN
    INSERT INTO coord.work_item (project_id, team_id, title, created_by)
         VALUES (gen_random_uuid(), gen_random_uuid(), 'dispatch item', 'user:alice')
      RETURNING id INTO wi;

    UPDATE coord.work_item
       SET requested_agent = 'coder', state = 'todo', updated_at = now()
     WHERE id = wi AND state = 'backlog';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 1, 'first dispatch must advance the backlog item (1 row)';

    SELECT state, requested_agent INTO st, ra FROM coord.work_item WHERE id = wi;
    ASSERT st = 'todo', 'dispatch must advance backlog→todo';
    ASSERT ra = 'coder', 'dispatch must stamp the requested agent';

    -- Re-dispatch: the item is no longer in backlog, so the CAS matches nothing.
    UPDATE coord.work_item
       SET requested_agent = 'other', state = 'todo', updated_at = now()
     WHERE id = wi AND state = 'backlog';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 're-dispatch of a non-backlog item must be a no-op (store maps to 409)';

    -- The audit row the store co-commits with the advance (append-only, §6.5).
    INSERT INTO coord.audit_log (work_item_id, event_type, principal, from_state, to_state, payload)
         VALUES (wi, 'work_item_dispatch_requested', 'user:alice', 'backlog', 'todo',
                 jsonb_build_object('requested_agent', 'coder', 'initiator', 'human'));
    ASSERT (SELECT count(*) FROM coord.audit_log
             WHERE work_item_id = wi AND event_type = 'work_item_dispatch_requested') = 1,
        'dispatch must leave one work_item_dispatch_requested audit row';
END $$;

ROLLBACK;
