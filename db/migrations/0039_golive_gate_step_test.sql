-- 0039_golive_gate_step_test.sql — runnable self-check for the WS-9 S5 go-live
-- gate's pre-dispatch hold step admission (ISI-5849, parent ISI-5818).
--
-- Same discipline as the 0005/0010/0017 self-checks: plain SQL, no framework,
-- run after the migrations it checks, inside one transaction ROLLED BACK at the
-- end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0002_coord_dispatch.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          -f db/migrations/0005_reconcile_step.sql \
--          -f db/migrations/0010_credential_pause.sql \
--          -f db/migrations/0017_reconcile_cancel_step.sql \
--          -f db/migrations/0039_golive_gate_step.sql \
--          -f db/migrations/0039_golive_gate_step_test.sql
--
-- The BEHAVIOURAL guarantees (pre-dispatch gate evaluation, any-of + fail-closed
-- admission, Secret-watch resume) are exercised by the Go tests (pkg/toolcred
-- oracle unit tests, pkg/coord ApplyGateHold, and the driver/coord integration
-- test). This file proves the *structural* AC of the schema this migration adds.

BEGIN;

-- (1) The reconcile_step CHECK now admits the S5 go-live gate hold step — the
--     driver's ApplyGateHold guarded UPDATE may land 'paused(credential_required)'
--     without a 23514 check_violation.
DO $$
DECLARE wi uuid;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'golive gate step item', 'principal:test')
      RETURNING id INTO wi;
    UPDATE coord.claim SET reconcile_step = 'paused(credential_required)' WHERE work_item_id = wi;
    ASSERT (SELECT reconcile_step FROM coord.claim WHERE work_item_id = wi) = 'paused(credential_required)',
        'a gate-held claim must hold reconcile_step = paused(credential_required)';
END $$;

-- (2) The pre-existing closed family still round-trips (the extension is a
--     superset, not a replacement): the 7.4/7.6 credential pause steps and the
--     3.3 kill step all remain admissible.
DO $$
DECLARE wi uuid;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'golive gate family item', 'principal:test')
      RETURNING id INTO wi;
    UPDATE coord.claim SET reconcile_step = 'paused(rate_limited)'         WHERE work_item_id = wi;
    UPDATE coord.claim SET reconcile_step = 'paused(credential_expired)'   WHERE work_item_id = wi;
    UPDATE coord.claim SET reconcile_step = 'paused(credential_rotated)'   WHERE work_item_id = wi;
    UPDATE coord.claim SET reconcile_step = 'paused(endpoint_unreachable)' WHERE work_item_id = wi;
    UPDATE coord.claim SET reconcile_step = 'cancelling'                   WHERE work_item_id = wi;
    ASSERT (SELECT reconcile_step FROM coord.claim WHERE work_item_id = wi) = 'cancelling',
        'the full pre-existing step family must remain admissible after the extension';
END $$;

-- (3) A garbage reconcile_step is still rejected (fail closed): drift to an
--     unclassifiable step fails with a check_violation.
DO $$
DECLARE wi uuid; blocked boolean;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'golive gate drift item', 'principal:test')
      RETURNING id INTO wi;
    blocked := false;
    BEGIN
        UPDATE coord.claim SET reconcile_step = 'paused(credential_require)' WHERE work_item_id = wi;
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'an out-of-family reconcile_step must be rejected by the CHECK';
END $$;

-- (4) The gate hold stays on the failover re-entry working set: the partial
--     index idx_claim_reconcile_active excludes only the terminal set, so a
--     'paused(credential_required)' claim is in the active scan (the driver's
--     resume re-kick finds it after a credential Secret lands).
DO $$
DECLARE wi uuid; active boolean;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'golive gate active-scan item', 'principal:test')
      RETURNING id INTO wi;
    UPDATE coord.claim SET reconcile_step = 'paused(credential_required)' WHERE work_item_id = wi;
    -- The index predicate is reconcile_step NOT IN (succeeded, failed, cancelled);
    -- mirror it here to assert the held claim is on the active working set.
    SELECT reconcile_step NOT IN ('succeeded','failed','cancelled') INTO active
      FROM coord.claim WHERE work_item_id = wi;
    ASSERT active, 'a gate-held claim must remain on the active failover re-entry scan set';
END $$;

ROLLBACK;
