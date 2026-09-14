-- 0020_work_item_create_fields_test.sql — runnable self-check for the create-time
-- attribute columns (ISI-4409). Same discipline as the 0018 self-check: plain SQL,
-- no framework, run after the migrations it checks, inside one transaction rolled
-- back at the end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          -f db/migrations/0005_reconcile_step.sql \
--          -f db/migrations/0020_work_item_create_fields.sql \
--          -f db/migrations/0020_work_item_create_fields_test.sql
--
-- The BEHAVIOURAL guarantees (Go validates enums before insert, empty ⇒ NULL/'{}')
-- are exercised by the Go tests (pkg/coord). This file proves the *structural* AC:
-- the columns exist, default to the honest "none" shape, and each CHECK rejects a
-- bad value while accepting NULL/valid.

BEGIN;

-- (1) The columns exist and default to the honest "none" shape on a fresh item:
--     priority/work_mode NULL, labels the empty array (never NULL).
DO $$
DECLARE wi uuid;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'create-fields defaults item', 'principal:test')
      RETURNING id INTO wi;
    ASSERT (SELECT priority  IS NULL FROM coord.work_item WHERE id = wi),
        'a fresh work item must carry priority NULL (honest "none")';
    ASSERT (SELECT work_mode IS NULL FROM coord.work_item WHERE id = wi),
        'a fresh work item must carry work_mode NULL (honest "none")';
    ASSERT (SELECT labels = '{}'::text[] FROM coord.work_item WHERE id = wi),
        'a fresh work item must carry labels ''{}'' (empty, never NULL)';
    ASSERT (SELECT labels IS NOT NULL FROM coord.work_item WHERE id = wi),
        'labels is NOT NULL — reads never null-check';
END $$;

-- (2) Valid enum values + a labels array are accepted and round-trip.
DO $$
DECLARE wi uuid;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by, priority, work_mode, labels)
         VALUES (gen_random_uuid(), 'create-fields valid item', 'principal:test',
                 'urgent', 'planning', ARRAY['backend', 'security'])
      RETURNING id INTO wi;
    ASSERT (SELECT priority  FROM coord.work_item WHERE id = wi) = 'urgent',
        'a valid priority must be stampable';
    ASSERT (SELECT work_mode FROM coord.work_item WHERE id = wi) = 'planning',
        'a valid work_mode must be stampable';
    ASSERT (SELECT labels FROM coord.work_item WHERE id = wi) = ARRAY['backend', 'security'],
        'labels must round-trip as a text[]';
END $$;

-- (3) Every priority enum member is accepted (guards against a typo'd CHECK list).
DO $$
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by, priority)
         VALUES (gen_random_uuid(), 'p-low',    'principal:test', 'low'),
                (gen_random_uuid(), 'p-medium', 'principal:test', 'medium'),
                (gen_random_uuid(), 'p-high',   'principal:test', 'high'),
                (gen_random_uuid(), 'p-urgent', 'principal:test', 'urgent');
END $$;

-- (4) The priority CHECK rejects a value outside the enum.
DO $$
BEGIN
    BEGIN
        INSERT INTO coord.work_item (project_id, title, created_by, priority)
             VALUES (gen_random_uuid(), 'bad priority', 'principal:test', 'critical');
        ASSERT false, 'priority CHECK must reject a value outside the enum';
    EXCEPTION WHEN check_violation THEN
        NULL; -- expected
    END;
END $$;

-- (5) The work_mode CHECK rejects a value outside the enum.
DO $$
BEGIN
    BEGIN
        INSERT INTO coord.work_item (project_id, title, created_by, work_mode)
             VALUES (gen_random_uuid(), 'bad work_mode', 'principal:test', 'yolo');
        ASSERT false, 'work_mode CHECK must reject a value outside the enum';
    EXCEPTION WHEN check_violation THEN
        NULL; -- expected
    END;
END $$;

ROLLBACK;
