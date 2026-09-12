-- 0017_reconcile_cancel_step_test.sql — runnable self-check for the 3.3
-- operator-kill transitional step admission (ISI-4299 defect 1) and the cancel
-- audit INSERT's uuid binding (ISI-4299 defect 2).
--
-- Same discipline as the 0005/0010 self-checks: plain SQL, no framework, run
-- after the migrations it checks, inside one transaction ROLLED BACK at the end
-- so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0002_coord_dispatch.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          -f db/migrations/0005_reconcile_step.sql \
--          -f db/migrations/0010_credential_pause.sql \
--          -f db/migrations/0017_reconcile_cancel_step.sql \
--          -f db/migrations/0017_reconcile_cancel_step_test.sql
--
-- The BEHAVIOURAL guarantees (fence-first Enter, terminal never resurrected,
-- checkout release, audit + outbox co-commit) are exercised by the Go tests
-- (internal/apiserver/killrun_test.go for the handler seam, pkg/coord chaos
-- gate for the store). This file proves the *structural* AC of the schema this
-- migration fixes.

BEGIN;

-- (1) The reconcile_step CHECK now admits the 3.3 transitional kill step —
--     CancelEnter's guarded UPDATE may land 'cancelling' without a 23514.
DO $$
DECLARE wi uuid;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'cancel step item', 'principal:test')
      RETURNING id INTO wi;
    UPDATE coord.claim SET reconcile_step = 'cancelling' WHERE work_item_id = wi;
    ASSERT (SELECT reconcile_step FROM coord.claim WHERE work_item_id = wi) = 'cancelling',
        'a kill-entered claim must hold reconcile_step = cancelling';
END $$;

-- (2) A garbage reconcile_step is still rejected (the extension is a superset,
--     not a hole): drift to an unclassifiable step fails closed.
DO $$
DECLARE wi uuid; blocked boolean;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'cancel drift item', 'principal:test')
      RETURNING id INTO wi;
    blocked := false;
    BEGIN
        UPDATE coord.claim SET reconcile_step = 'cancellling' WHERE work_item_id = wi;
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'an out-of-family reconcile_step must be rejected by the CHECK';
END $$;

-- (3) ISI-4299 defect 2, schema side: the FIXED audit binding
--     NULLIF(:initiated_by,'')::uuid parses and inserts against the shipped
--     audit_log — a resolved auth.user.id uuid lands, and the empty (unknown
--     initiator) form stores NULL rather than tripping 42804.
DO $$
DECLARE wi uuid; usr uuid; n int;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'cancel audit item', 'principal:test')
      RETURNING id INTO wi;
    usr := gen_random_uuid();

    -- The exact expression shape cancelprod.go binds (text param, explicit cast).
    INSERT INTO coord.audit_log
           (work_item_id, run_id, event_type, principal,
            initiated_by_user_id, fence_token, to_state)
    VALUES (wi, NULLIF('','')::uuid, 'cancel_requested', 'ksquad-apiserver',
            NULLIF(usr::text,'')::uuid, 1, 'cancelling');

    INSERT INTO coord.audit_log
           (work_item_id, run_id, event_type, principal,
            initiated_by_user_id, fence_token, to_state)
    VALUES (wi, NULLIF('','')::uuid, 'run_cancelled', 'ksquad-apiserver',
            NULLIF('','')::uuid, 2, 'cancelled');

    SELECT count(*) INTO n FROM coord.audit_log
     WHERE work_item_id = wi AND event_type IN ('cancel_requested','run_cancelled');
    ASSERT n = 2, 'both cancel audit rows must land';

    ASSERT (SELECT initiated_by_user_id FROM coord.audit_log
             WHERE work_item_id = wi AND event_type = 'cancel_requested') = usr,
        'the resolved user uuid must be stored verbatim';
    ASSERT (SELECT initiated_by_user_id IS NULL FROM coord.audit_log
             WHERE work_item_id = wi AND event_type = 'run_cancelled'),
        'an empty initiator must store NULL, not fail the insert';
END $$;

-- (4) ISI-4299 defect 2, negative pin: a non-uuid initiator (a principal
--     string like 'user:admin') is rejected by the uuid column — the Go layer
--     must resolve the initiator to auth.user.id, never pass the principal.
DO $$
DECLARE wi uuid; blocked boolean;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'cancel audit reject item', 'principal:test')
      RETURNING id INTO wi;
    blocked := false;
    BEGIN
        INSERT INTO coord.audit_log
               (work_item_id, event_type, principal, initiated_by_user_id, to_state)
        VALUES (wi, 'cancel_requested', 'ksquad-apiserver', 'user:admin', 'cancelling');
    EXCEPTION WHEN invalid_text_representation THEN blocked := true;
    END;
    ASSERT blocked, 'a non-uuid initiator must be rejected (22P02), proving the handler must resolve the user id';
END $$;

ROLLBACK;
