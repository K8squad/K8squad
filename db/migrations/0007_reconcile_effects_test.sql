-- 0007_reconcile_effects_test.sql — runnable self-check for the warm-pool
-- sandbox-bind idempotency marker (Story 3.1 / ISI-2655, child ISI-2802).
--
-- Same discipline as the 0001/0005 self-checks: plain SQL, no framework, run after
-- the migrations it checks, inside one transaction ROLLED BACK at the end so it
-- leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0002_coord_dispatch.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          -f db/migrations/0005_reconcile_step.sql \
--          -f db/migrations/0007_reconcile_effects.sql \
--          -f db/migrations/0007_reconcile_effects_test.sql
--
-- The BEHAVIOURAL guarantees (at-most-once bind/dispatch/upsert under re-entry, the
-- effects co-driving the machine over the real Store) are exercised by the Go
-- integration gate (TestProdEffects, pkg/coord/prod_effects_chaos_test.go,
-- -tags=chaos). This file proves the *structural* AC of the schema this child adds.

BEGIN;

-- (1) coord.sandbox_bind exists with run_id as the PK idempotency key and the
--     custody columns (work_item_id FK, sandbox_ref, bound_by).
DO $$
DECLARE pk_col text; has_ref int; has_by int; fk int;
BEGIN
    SELECT a.attname INTO pk_col
      FROM pg_index i
      JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
     WHERE i.indrelid = 'coord.sandbox_bind'::regclass AND i.indisprimary;
    ASSERT pk_col = 'run_id', format('sandbox_bind PK must be run_id, got %s', pk_col);

    SELECT count(*) INTO has_ref FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='sandbox_bind' AND column_name='sandbox_ref';
    ASSERT has_ref = 1, 'coord.sandbox_bind.sandbox_ref must exist';

    SELECT count(*) INTO has_by FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='sandbox_bind' AND column_name='bound_by';
    ASSERT has_by = 1, 'coord.sandbox_bind.bound_by (§6.5 provenance) must exist';

    SELECT count(*) INTO fk
      FROM information_schema.table_constraints
     WHERE table_schema='coord' AND table_name='sandbox_bind' AND constraint_type='FOREIGN KEY';
    ASSERT fk >= 1, 'coord.sandbox_bind must reference coord.work_item';
END $$;

-- (2) The run_id PK is a hard dedup guard — a re-driven bind on the same run_id is
--     rejected, and ON CONFLICT DO NOTHING (the intended re-drive path) is a clean
--     no-op that never double-provisions.
DO $$
DECLARE wi uuid; run uuid; blocked boolean;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'sandbox bind item', 'principal:test')
      RETURNING id INTO wi;
    run := '33333333-3333-3333-3333-333333333333'::uuid;

    INSERT INTO coord.sandbox_bind (run_id, work_item_id, sandbox_ref, bound_by)
         VALUES (run, wi, 'sbx-abc', 'principal:test');

    blocked := false;
    BEGIN
        INSERT INTO coord.sandbox_bind (run_id, work_item_id, sandbox_ref, bound_by)
             VALUES (run, wi, 'sbx-def', 'principal:test');
    EXCEPTION WHEN unique_violation THEN blocked := true;
    END;
    ASSERT blocked, 'duplicate run_id must be rejected (at-most-once sandbox bind)';

    -- ON CONFLICT DO NOTHING is the re-drive path: a clean no-op leaving the FIRST
    -- bind's sandbox_ref intact (reattach, never re-provision).
    INSERT INTO coord.sandbox_bind (run_id, work_item_id, sandbox_ref, bound_by)
         VALUES (run, wi, 'sbx-def', 'principal:test')
    ON CONFLICT (run_id) DO NOTHING;

    ASSERT (SELECT sandbox_ref FROM coord.sandbox_bind WHERE run_id = run) = 'sbx-abc',
        're-drive must reattach to the first bind, not overwrite the sandbox_ref';
END $$;

-- (3) sandbox_ref DEFAULTs to '' so the ledger-only mode (bind marker recorded
--     before the physical warm-pool adapter lands) inserts without a handle.
DO $$
DECLARE def text;
BEGIN
    SELECT column_default INTO def FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='sandbox_bind' AND column_name='sandbox_ref';
    ASSERT def IS NOT NULL, 'sandbox_ref must have a DEFAULT for ledger-only mode';
END $$;

ROLLBACK;
