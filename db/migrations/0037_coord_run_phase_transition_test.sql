-- 0037_coord_run_phase_transition_test.sql — runnable self-check for ISI-5746 (WS-1 of ISI-5744).
--
-- Same no-framework discipline as 0015/0029: plain SQL that fails loudly if the run phase-transition
-- journal or its append-only contract breaks. Runs AFTER the full migration set, inside one
-- transaction ROLLED BACK at the end (leaves no residue):
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/0001_coord_schema.sql ... \
--                                              -f db/migrations/0037_coord_run_phase_transition.sql \
--                                              -f db/migrations/0037_coord_run_phase_transition_test.sql

BEGIN;

-- (1) The table exists in the coord schema with the required columns/types.
DO $$
DECLARE t text; n text;
BEGIN
    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='run_phase_transition' AND column_name='run_uid';
    ASSERT t = 'uuid', format('expected coord.run_phase_transition.run_uid uuid, found %s', coalesce(t,'<absent>'));

    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='run_phase_transition' AND column_name='project_id';
    ASSERT t = 'uuid', format('expected coord.run_phase_transition.project_id uuid, found %s', coalesce(t,'<absent>'));

    -- project_id is NOT NULL (tenancy predicate); team_id is nullable (unresolved Team still records).
    SELECT is_nullable INTO n FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='run_phase_transition' AND column_name='project_id';
    ASSERT n = 'NO', format('expected project_id NOT NULL, found is_nullable=%s', coalesce(n,'<absent>'));
    SELECT is_nullable INTO n FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='run_phase_transition' AND column_name='team_id';
    ASSERT n = 'YES', format('expected team_id nullable, found is_nullable=%s', coalesce(n,'<absent>'));
END $$;

-- (2) A valid transition round-trips; the first edge carries a NULL from_phase.
DO $$
DECLARE f text; tp text; r uuid;
BEGIN
    INSERT INTO coord.run_phase_transition (run_uid, team_id, project_id, from_phase, to_phase)
    VALUES ('11111111-1111-1111-1111-111111111111',
            '22222222-2222-2222-2222-222222222222',
            '33333333-3333-3333-3333-333333333333',
            NULL, 'claiming');
    INSERT INTO coord.run_phase_transition (run_uid, team_id, project_id, from_phase, to_phase)
    VALUES ('11111111-1111-1111-1111-111111111111',
            '22222222-2222-2222-2222-222222222222',
            '33333333-3333-3333-3333-333333333333',
            'claiming', 'running');

    SELECT from_phase, to_phase, run_uid INTO f, tp, r
      FROM coord.run_phase_transition
     WHERE run_uid='11111111-1111-1111-1111-111111111111' ORDER BY id ASC LIMIT 1;
    ASSERT f IS NULL, format('first edge from_phase should be NULL, found %s', coalesce(f,'<null-ok>'));
    ASSERT tp = 'claiming', format('first edge to_phase mismatch, found %s', tp);

    -- A terminal edge with a non-null from_phase round-trips too.
    INSERT INTO coord.run_phase_transition (run_uid, project_id, from_phase, to_phase)
    VALUES ('11111111-1111-1111-1111-111111111111',
            '33333333-3333-3333-3333-333333333333',
            'running', 'complete');
    SELECT to_phase INTO tp FROM coord.run_phase_transition
     WHERE run_uid='11111111-1111-1111-1111-111111111111' ORDER BY id DESC LIMIT 1;
    ASSERT tp = 'complete', format('terminal to_phase mismatch, found %s', tp);
END $$;

-- (3) The phase enum rejects anything outside the four chip tokens (both from_phase and to_phase).
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        INSERT INTO coord.run_phase_transition (run_uid, project_id, to_phase)
        VALUES ('44444444-4444-4444-4444-444444444444',
                '33333333-3333-3333-3333-333333333333', 'Succeeded'); -- raw CR phase, not a chip token
    EXCEPTION WHEN check_violation THEN
        ok := true;
    END;
    ASSERT ok, 'to_phase accepted a value outside (claiming,running,complete,failed)';

    ok := false;
    BEGIN
        INSERT INTO coord.run_phase_transition (run_uid, project_id, from_phase, to_phase)
        VALUES ('44444444-4444-4444-4444-444444444444',
                '33333333-3333-3333-3333-333333333333', 'bogus', 'running');
    EXCEPTION WHEN check_violation THEN
        ok := true;
    END;
    ASSERT ok, 'from_phase accepted a value outside (claiming,running,complete,failed)';
END $$;

-- (4) project_id is required (tenancy predicate): a NULL project_id is rejected.
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        INSERT INTO coord.run_phase_transition (run_uid, to_phase)
        VALUES ('44444444-4444-4444-4444-444444444444', 'claiming');
    EXCEPTION WHEN not_null_violation THEN
        ok := true;
    END;
    ASSERT ok, 'project_id NULL was accepted (tenancy predicate must be required)';
END $$;

-- (5) Append-only: UPDATE and DELETE are rejected by the trigger (restrict_violation).
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        UPDATE coord.run_phase_transition SET to_phase='failed'
         WHERE run_uid='11111111-1111-1111-1111-111111111111';
    EXCEPTION WHEN restrict_violation THEN
        ok := true;
    END;
    ASSERT ok, 'UPDATE on coord.run_phase_transition was not rejected (append-only breach)';

    ok := false;
    BEGIN
        DELETE FROM coord.run_phase_transition
         WHERE run_uid='11111111-1111-1111-1111-111111111111';
    EXCEPTION WHEN restrict_violation THEN
        ok := true;
    END;
    ASSERT ok, 'DELETE on coord.run_phase_transition was not rejected (append-only breach)';
END $$;

-- (6) TRUNCATE cannot silently wipe the journal (the F2 evasion the statement trigger closes).
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        TRUNCATE coord.run_phase_transition;
    EXCEPTION WHEN restrict_violation THEN
        ok := true;
    END;
    ASSERT ok, 'TRUNCATE on coord.run_phase_transition was not rejected (append-only breach)';
END $$;

ROLLBACK;
