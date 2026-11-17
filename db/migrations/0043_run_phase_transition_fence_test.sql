-- 0043_run_phase_transition_fence_test.sql — runnable self-check for the fence
-- column (ISI-7541). Same discipline as 0037's companion: plain SQL, loud
-- ASSERTs, wrapped in BEGIN/ROLLBACK so it leaves no residue.
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/0001_coord_schema.sql --          ... -f db/migrations/0042_*.sql -f db/migrations/0043_run_phase_transition_fence.sql --          -f db/migrations/0043_run_phase_transition_fence_test.sql

BEGIN;

-- (1) The column exists with the right type and nullability.
DO $$
DECLARE t text; n text;
BEGIN
    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='run_phase_transition' AND column_name='fence';
    ASSERT t = 'bigint', format('expected coord.run_phase_transition.fence bigint, found %s', coalesce(t,'<absent>'));

    SELECT is_nullable INTO n FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='run_phase_transition' AND column_name='fence';
    ASSERT n = 'YES', format('expected fence nullable, found is_nullable=%s', coalesce(n,'<absent>'));
END $$;

-- (2) A guard append round-trips: an explicit fence stamps, an omitted fence is NULL.
DO $$
DECLARE f bigint;
BEGIN
    INSERT INTO coord.run_phase_transition (run_uid, project_id, from_phase, to_phase, fence)
    VALUES ('55555555-5555-5555-5555-555555555555',
            '33333333-3333-3333-3333-333333333333', 'running', 'claiming', 5);
    SELECT fence INTO f FROM coord.run_phase_transition
     WHERE run_uid='55555555-5555-5555-5555-555555555555' ORDER BY id DESC LIMIT 1;
    ASSERT f = 5, format('fence round-trip mismatch, found %s', f);

    INSERT INTO coord.run_phase_transition (run_uid, project_id, to_phase)
    VALUES ('66666666-6666-6666-6666-666666666666',
            '33333333-3333-3333-3333-333333333333', 'claiming');
    SELECT fence INTO f FROM coord.run_phase_transition
     WHERE run_uid='66666666-6666-6666-6666-666666666666' ORDER BY id DESC LIMIT 1;
    ASSERT f IS NULL, format('omitted fence should round-trip NULL, found %s', f);
END $$;

ROLLBACK;
