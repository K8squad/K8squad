-- 0010_credential_pause_test.sql — runnable self-check for the per-credential
-- pause/resume ledger + the reconcile_step enum extension (Stories 7.4+7.6 /
-- ISI-2898).
--
-- Same discipline as the 0005/0007 self-checks: plain SQL, no framework, run
-- after the migrations it checks, inside one transaction ROLLED BACK at the end
-- so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0002_coord_dispatch.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          -f db/migrations/0005_reconcile_step.sql \
--          -f db/migrations/0010_credential_pause.sql \
--          -f db/migrations/0010_credential_pause_test.sql
--
-- The BEHAVIOURAL guarantees (per-credential exactly-once timer resume,
-- refresh-mode clearing, the atomic ApplyCredentialSignal co-commit) are
-- exercised by the Go chaos gate (TestSpineCredentialPause,
-- pkg/coord/credpause_chaos_test.go, -tags=chaos). This file proves the
-- *structural* AC of the schema this migration adds.

BEGIN;

-- (1) coord.credential_pause exists with credential_id as the PK attribution key.
DO $$
DECLARE pk_col text; has_reason int; has_resume int;
BEGIN
    SELECT a.attname INTO pk_col
      FROM pg_index i
      JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
     WHERE i.indrelid = 'coord.credential_pause'::regclass AND i.indisprimary;
    ASSERT pk_col = 'credential_id',
        format('credential_pause PK must be credential_id, got %s', pk_col);

    SELECT count(*) INTO has_reason FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='credential_pause' AND column_name='reason';
    ASSERT has_reason = 1, 'coord.credential_pause.reason must exist';

    SELECT count(*) INTO has_resume FROM information_schema.columns
     WHERE table_schema='coord' AND table_name='credential_pause' AND column_name='resume_at';
    ASSERT has_resume = 1, 'coord.credential_pause.resume_at must exist';
END $$;

-- (2) The reconcile_step CHECK now admits the full credential pause family — a
--     claim can be held in any of the three new steps (7.4) without violating it.
DO $$
DECLARE wi uuid;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'credpause step item', 'principal:test')
      RETURNING id INTO wi;
    -- coord.claim carries reconcile_step; a paused(credential_*) value must be accepted.
    UPDATE coord.claim SET reconcile_step = 'paused(credential_expired)' WHERE work_item_id = wi;
    UPDATE coord.claim SET reconcile_step = 'paused(credential_rotated)' WHERE work_item_id = wi;
    UPDATE coord.claim SET reconcile_step = 'paused(endpoint_unreachable)' WHERE work_item_id = wi;
    UPDATE coord.claim SET reconcile_step = 'paused(rate_limited)' WHERE work_item_id = wi;
END $$;

-- (3) A garbage reconcile_step is still rejected (the CHECK is a superset, not a
--     hole): drift to an unclassifiable step fails closed.
DO $$
DECLARE wi uuid; blocked boolean;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'credpause drift item', 'principal:test')
      RETURNING id INTO wi;
    blocked := false;
    BEGIN
        UPDATE coord.claim SET reconcile_step = 'paused(gremlin)' WHERE work_item_id = wi;
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'an out-of-family reconcile_step must be rejected by the CHECK';
END $$;

-- (4) credential_id PK makes the attribution idempotent: a second episode for the
--     same credential must be an ON CONFLICT rewrite, not a duplicate row.
DO $$
DECLARE blocked boolean; n int;
BEGIN
    INSERT INTO coord.credential_pause
        (credential_id, principal, credential_class, reason, attempt, paused_at)
        VALUES ('team-a/alice-oauth', 'alice', 'claude_oauth', 'credential_expired', 1, clock_timestamp());

    blocked := false;
    BEGIN
        INSERT INTO coord.credential_pause
            (credential_id, principal, credential_class, reason, attempt, paused_at)
            VALUES ('team-a/alice-oauth', 'alice', 'claude_oauth', 'credential_rotated', 1, clock_timestamp());
    EXCEPTION WHEN unique_violation THEN blocked := true;
    END;
    ASSERT blocked, 'duplicate credential_id must be rejected (one live episode per credential)';

    -- ON CONFLICT DO UPDATE is the re-signal path: reason rewrites in place.
    INSERT INTO coord.credential_pause
        (credential_id, principal, credential_class, reason, attempt, paused_at)
        VALUES ('team-a/alice-oauth', 'alice', 'claude_oauth', 'credential_rotated', 2, clock_timestamp())
    ON CONFLICT (credential_id) DO UPDATE SET reason = EXCLUDED.reason, attempt = EXCLUDED.attempt;

    SELECT count(*) INTO n FROM coord.credential_pause WHERE credential_id = 'team-a/alice-oauth';
    ASSERT n = 1, 're-signal must rewrite the single episode in place, not duplicate it';
    ASSERT (SELECT reason FROM coord.credential_pause WHERE credential_id = 'team-a/alice-oauth')
           = 'credential_rotated', 'ON CONFLICT rewrite must land the newest reason';
END $$;

-- (5) 7.4 teeth: the timer-only-rate-limited CHECK forbids fabricating a clock
--     wake for a refresh-mode hold — a credential_expired row with a resume_at is
--     rejected, and an endpoint_unreachable row with a retry_after_ms is rejected.
DO $$
DECLARE blocked boolean;
BEGIN
    blocked := false;
    BEGIN
        INSERT INTO coord.credential_pause
            (credential_id, credential_class, reason, attempt, resume_at, paused_at)
            VALUES ('team-b/bob-key', 'api_key', 'credential_expired', 1,
                    clock_timestamp() + interval '30s', clock_timestamp());
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'a refresh-mode hold with a resume_at timer must be rejected (7.4)';

    blocked := false;
    BEGIN
        INSERT INTO coord.credential_pause
            (credential_id, credential_class, reason, attempt, retry_after_ms, paused_at)
            VALUES ('team-b/bob-endpoint', 'byo_endpoint', 'endpoint_unreachable', 1,
                    5000, clock_timestamp());
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'a refresh-mode hold with a retry_after_ms must be rejected (7.4)';

    -- The same shapes are valid for rate_limited (timer resume is legitimate).
    INSERT INTO coord.credential_pause
        (credential_id, credential_class, reason, attempt, retry_after_ms, resume_at, paused_at)
        VALUES ('team-b/bob-rl', 'api_key', 'rate_limited', 1, 5000,
                clock_timestamp() + interval '5s', clock_timestamp());
END $$;

-- (6) The class and reason enums fail closed on an out-of-model value.
DO $$
DECLARE blocked boolean;
BEGIN
    blocked := false;
    BEGIN
        INSERT INTO coord.credential_pause
            (credential_id, credential_class, reason, attempt, paused_at)
            VALUES ('team-c/x', 'kerberos', 'credential_expired', 1, clock_timestamp());
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'an out-of-model credential_class must be rejected';

    blocked := false;
    BEGIN
        INSERT INTO coord.credential_pause
            (credential_id, credential_class, reason, attempt, paused_at)
            VALUES ('team-c/y', 'api_key', 'quota_exceeded', 1, clock_timestamp());
    EXCEPTION WHEN check_violation THEN blocked := true;
    END;
    ASSERT blocked, 'an out-of-family reason must be rejected';
END $$;

ROLLBACK;
