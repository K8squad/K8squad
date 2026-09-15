-- 0019_work_item_phase_states_test.sql — runnable self-check for the phase-status
-- enum extension (ISI-4455). Same discipline as the 0017 self-check: plain SQL,
-- no framework, run after the migrations it checks, inside one transaction ROLLED
-- BACK at the end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0019_work_item_phase_states.sql \
--          -f db/migrations/0019_work_item_phase_states_test.sql
--
-- The BEHAVIOURAL guarantees (human move graph, no-fence audit) are exercised by
-- the Go tests (pkg/coord/humanstate_test.go, internal/apiserver). This file
-- proves the STRUCTURAL AC of the schema this migration changes.

BEGIN;

-- (1) Each of the 6 net-new phase columns + cancelled is ADMITTED — a human move
--     into them lands without a 23514.
DO $$
DECLARE
    wi uuid;
    s  text;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'phase enum item', 'principal:test')
      RETURNING id INTO wi;
    FOREACH s IN ARRAY ARRAY['design','planning','implementation',
                             'code_review','testing','documentation','cancelled']
    LOOP
        UPDATE coord.work_item SET state = s WHERE id = wi;
        ASSERT (SELECT state FROM coord.work_item WHERE id = wi) = s,
            format('phase state %L must be admitted by the CHECK', s);
    END LOOP;
END $$;

-- (2) The retained engine lanes still validate (additive, not a replace): the
--     coordinator's mechanical dispatch writes must keep working.
DO $$
DECLARE
    wi uuid;
    s  text;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'engine lane item', 'principal:test')
      RETURNING id INTO wi;
    FOREACH s IN ARRAY ARRAY['backlog','todo','in_progress','in_review','done']
    LOOP
        UPDATE coord.work_item SET state = s WHERE id = wi;
        ASSERT (SELECT state FROM coord.work_item WHERE id = wi) = s,
            format('retained lane %L must still validate', s);
    END LOOP;
END $$;

-- (3) A garbage state is STILL rejected (the extension is a superset, not a
--     hole): drift to an unclassifiable lane fails closed.
DO $$
DECLARE
    wi uuid;
    ok boolean := false;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'bad state item', 'principal:test')
      RETURNING id INTO wi;
    BEGIN
        UPDATE coord.work_item SET state = 'nonsense_lane' WHERE id = wi;
    EXCEPTION WHEN check_violation THEN
        ok := true;
    END;
    ASSERT ok, 'an unknown state must still be rejected by the CHECK (fail-closed)';
END $$;

ROLLBACK;
