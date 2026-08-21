-- 0009_rate_limit_reroute_test.sql — runnable self-check for the Story 2.10
-- throttled-credential hold (ISI-2882).
--
-- Same discipline as 0001/0002/0005 self-checks: plain SQL, no framework, run
-- after the migrations it checks, inside one transaction ROLLED BACK at the end
-- so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0009_rate_limit_reroute.sql \
--          -f db/migrations/0009_rate_limit_reroute_test.sql
--
-- The BEHAVIOURAL guarantees (fenced release, different-credential claim guard,
-- hold expiry, idempotent re-release) are exercised by the Go chaos gate
-- (TestSpineReroute, pkg/coord/prod_reroute_chaos_test.go, -tags=chaos). This
-- file proves the *structural* AC of the schema this story adds.

BEGIN;

-- (1) The hold row exists with the Story 2.10 shape, and a hold can be written
--     and read back with the credential identity + window the guard reads.
DO $$
DECLARE wi uuid; hold_cred text; hold_resume timestamptz;
BEGIN
    INSERT INTO coord.work_item (project_id, title, state, created_by)
         VALUES (gen_random_uuid(), 'reroute hold item', 'in_progress', 'principal:test')
      RETURNING id INTO wi;
    INSERT INTO coord.rate_limit_reroute
        (work_item_id, throttled_credential, attempt, resume_at,
         released_fence, released_run, coordinator)
    VALUES (wi, 'secret:squad/cred-a', 2, now() + interval '5 minutes',
            8, gen_random_uuid(), 'principal:coord');
    SELECT throttled_credential, resume_at INTO hold_cred, hold_resume
      FROM coord.rate_limit_reroute WHERE work_item_id = wi;
    ASSERT hold_cred = 'secret:squad/cred-a', 'hold must store the throttled credential key';
    ASSERT hold_resume > now(), 'hold resume_at must be the future window clear';
END $$;

-- (2) The hold is ONE row per work item, rewritten in place on a subsequent
--     escalation (the claim-row discipline) — the PK is the structural guard.
DO $$
DECLARE wi uuid; blocked boolean;
BEGIN
    SELECT work_item_id INTO wi FROM coord.rate_limit_reroute LIMIT 1;
    blocked := false;
    BEGIN
        INSERT INTO coord.rate_limit_reroute
            (work_item_id, throttled_credential, attempt, resume_at,
             released_fence, released_run, coordinator)
        VALUES (wi, 'secret:squad/cred-b', 3, now() + interval '9 minutes',
                11, gen_random_uuid(), 'principal:coord');
    EXCEPTION WHEN unique_violation THEN blocked := true;
    END;
    ASSERT blocked, 'a second hold row per work item must be rejected (PK guard)';

    -- ON CONFLICT DO UPDATE is the intended re-escalation path: rewrite in place.
    INSERT INTO coord.rate_limit_reroute
        (work_item_id, throttled_credential, attempt, resume_at,
         released_fence, released_run, coordinator)
    VALUES (wi, 'secret:squad/cred-b', 3, now() + interval '9 minutes',
            11, gen_random_uuid(), 'principal:coord')
    ON CONFLICT (work_item_id) DO UPDATE SET
        throttled_credential = EXCLUDED.throttled_credential,
        attempt              = EXCLUDED.attempt,
        resume_at            = EXCLUDED.resume_at,
        released_fence       = EXCLUDED.released_fence,
        released_run         = EXCLUDED.released_run,
        coordinator          = EXCLUDED.coordinator;
END $$;

-- (3) The rewrite path rides the canonical 0001 touch trigger: the BEFORE
--     UPDATE trigger must be attached (updated_at maintenance on the rewrite
--     is the trigger's job — a behavioral >created_at assertion is not
--     expressible here because now() is frozen within this one transaction).
DO $$
DECLARE triggers int;
BEGIN
    SELECT count(*) INTO triggers FROM pg_trigger
     WHERE tgrelid = 'coord.rate_limit_reroute'::regclass
       AND tgname = 'rate_limit_reroute_touch_updated_at'
       AND NOT tgisinternal;
    ASSERT triggers = 1, 'the 0001 touch_updated_at trigger must be attached to the hold rewrite';
END $$;

ROLLBACK;
