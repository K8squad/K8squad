-- 0017_claim_assignee_test.sql — runnable self-check for the agent-attribution
-- column (ISI-4237). Same discipline as the 0016 self-check: plain SQL, no
-- framework, run after the migrations it checks, inside one transaction rolled
-- back at the end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0001_coord_schema.sql \
--          -f db/migrations/0003_coord_outbox.sql \
--          -f db/migrations/0005_reconcile_step.sql \
--          -f db/migrations/0017_claim_assignee.sql \
--          -f db/migrations/0017_claim_assignee_test.sql
--
-- The BEHAVIOURAL guarantees (acquire stamps it, release retains it) are
-- exercised by the Go tests (pkg/coord). This file proves the *structural*
-- AC: the column exists, defaults NULL, and coexists with the custody guard.

BEGIN;

-- (1) The column exists and defaults NULL on a fresh work item (provisioned
--     claim row): attribution is absent until an acquire stamps it.
DO $$
DECLARE wi uuid;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'assignee column item', 'principal:test')
      RETURNING id INTO wi;
    ASSERT (SELECT assignee_agent IS NULL FROM coord.claim WHERE work_item_id = wi),
        'a fresh claim row must carry assignee_agent NULL';
END $$;

-- (2) The column is writable and independent of the custody guard: stamping it
--     does not move holder/fence/lease, and releasing the checkout (the
--     terminal-settle shape) does not clear it.
DO $$
DECLARE wi uuid;
BEGIN
    INSERT INTO coord.work_item (project_id, title, created_by)
         VALUES (gen_random_uuid(), 'assignee stamp item', 'principal:test')
      RETURNING id INTO wi;
    UPDATE coord.claim
       SET assignee_agent = 'sam', holder_principal = 'ksquad-operator',
           fence_token = fence_token + 1
     WHERE work_item_id = wi;
    ASSERT (SELECT assignee_agent FROM coord.claim WHERE work_item_id = wi) = 'sam',
        'assignee_agent must be stampable';
    -- the terminal release shape: holder/lease cleared, attribution retained
    UPDATE coord.claim
       SET holder_principal = NULL, lease_expires_at = NULL
     WHERE work_item_id = wi;
    ASSERT (SELECT assignee_agent FROM coord.claim WHERE work_item_id = wi) = 'sam',
        'assignee_agent must survive the terminal checkout release';
    ASSERT (SELECT holder_principal IS NULL FROM coord.claim WHERE work_item_id = wi),
        'the release shape itself is unchanged';
END $$;

ROLLBACK;
