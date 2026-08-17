-- 0005_reconcile_step_test.sql — runnable self-check for the reconcile_step +
-- reclaim marker + canonical-outbox projection + A2A dispatch marker
-- (Story 3.1 / ISI-2655).
--
-- Same discipline as 0001/0002 self-checks: plain SQL, no framework, run after the
-- migrations it checks, inside one transaction ROLLED BACK at the end so it leaves
-- no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0002_coord_dispatch.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          -f db/migrations/0005_reconcile_step.sql \
--          -f db/migrations/0005_reconcile_step_test.sql
--
-- The BEHAVIOURAL guarantees (exactly-once advance under fence + step-CAS, audit +
-- outbox co-commit, monotonic reclaim) are exercised by the Go integration gate
-- (TestProdReconcile, pkg/coord/prod_reconcile_chaos_test.go, -tags=chaos). This
-- file proves the *structural* AC of the schema this story adds.

BEGIN;

-- (1) coord.claim carries reconcile_step (NOT NULL, defaults 'pending') and the
--     §6.3 reclaim marker reclaim_fenced_at (nullable).
DO $$
DECLARE step_default text; step_notnull boolean; has_reclaim int;
BEGIN
    SELECT column_default, is_nullable = 'NO'
      INTO step_default, step_notnull
      FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='claim' AND column_name='reconcile_step';
    ASSERT step_default IS NOT NULL AND step_default LIKE '%pending%',
        format('reconcile_step must default to pending, got %s', step_default);
    ASSERT step_notnull, 'reconcile_step must be NOT NULL';

    SELECT count(*) INTO has_reclaim FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='claim' AND column_name='reclaim_fenced_at';
    ASSERT has_reclaim = 1, 'coord.claim.reclaim_fenced_at (§6.3 marker) must exist';
END $$;

-- (2) A pre-provisioned claim row starts at 'pending' with no extra write (the
--     work_item insert trigger creates it; the column default carries the step).
DO $$
DECLARE wi uuid; step text;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'reconcile item', 'principal:test')
      RETURNING id INTO wi;
    SELECT reconcile_step INTO step FROM coord.claim WHERE work_item_id = wi;
    ASSERT step = 'pending', format('fresh claim must start pending, got %s', step);
END $$;

-- (3) The reconcile_step CHECK rejects a value the machine cannot classify — a
--     stuck-Run defect is refused at the source.
DO $$
DECLARE wi uuid; blocked boolean;
BEGIN
    SELECT work_item_id INTO wi FROM coord.claim LIMIT 1;
    blocked := false;
    BEGIN
        UPDATE coord.claim SET reconcile_step = 'not_a_real_step' WHERE work_item_id = wi;
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'reconcile_step CHECK must reject a non-enum step';
END $$;

-- (4) The reconcile-advance projection rides the CANONICAL coord.outbox
--     (0003_coord_outbox): an entity='run' / event_type='reconcile_advanced' event
--     whose from_step/to_step/fence_token ride in the jsonb payload is accepted,
--     published_at is settable (relay flush marker), and the append-only guard
--     rejects DELETE. This is the same schema ProdReconcileStore.Advance writes.
DO $$
DECLARE wi uuid; pid uuid; oid bigint; blocked boolean;
BEGIN
    SELECT c.work_item_id, w.project_id
      INTO wi, pid
      FROM coord.claim c JOIN coord.work_item w ON w.id = c.work_item_id
     LIMIT 1;
    INSERT INTO coord.outbox (entity, project_id, squad, event_type,
                              work_item_id, run_id, payload)
         VALUES ('run', pid, NULL, 'reconcile_advanced', wi, gen_random_uuid(),
                 jsonb_build_object('from_step', 'pending',
                                    'to_step', 'claiming_sandbox',
                                    'fence_token', 1))
      RETURNING id INTO oid;

    -- UPDATE of published_at is ALLOWED (the relay marks rows published).
    UPDATE coord.outbox SET published_at = now() WHERE id = oid;

    -- DELETE is rejected by the canonical append-only guard (§17.4).
    blocked := false;
    BEGIN
        DELETE FROM coord.outbox WHERE id = oid;
    EXCEPTION WHEN OTHERS THEN blocked := true;   -- restrict_violation raised by the outbox guard
    END;
    ASSERT blocked, 'coord.outbox DELETE must be rejected (append-only §17.4)';
END $$;

-- (5) coord.a2a_dispatch: the deterministic a2a_task_id is a hard PK guard — a
--     re-driven dispatch on the same task id is a no-op, not a second execution.
DO $$
DECLARE wi uuid; blocked boolean;
BEGIN
    SELECT work_item_id INTO wi FROM coord.claim LIMIT 1;
    INSERT INTO coord.a2a_dispatch (a2a_task_id, work_item_id, run_id)
         VALUES ('11111111-1111-1111-1111-111111111111', wi,
                 '11111111-1111-1111-1111-111111111111'::uuid);
    blocked := false;
    BEGIN
        INSERT INTO coord.a2a_dispatch (a2a_task_id, work_item_id, run_id)
             VALUES ('11111111-1111-1111-1111-111111111111', wi,
                     '11111111-1111-1111-1111-111111111111'::uuid);
    EXCEPTION WHEN unique_violation THEN blocked := true;
    END;
    ASSERT blocked, 'duplicate a2a_task_id must be rejected (deterministic dispatch dedup)';

    -- ON CONFLICT DO NOTHING is the intended re-drive path: it must be a clean no-op.
    INSERT INTO coord.a2a_dispatch (a2a_task_id, work_item_id, run_id)
         VALUES ('11111111-1111-1111-1111-111111111111', wi,
                 '11111111-1111-1111-1111-111111111111'::uuid)
    ON CONFLICT (a2a_task_id) DO NOTHING;
END $$;

ROLLBACK;
