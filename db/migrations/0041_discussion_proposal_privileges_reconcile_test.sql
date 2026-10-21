-- 0041_discussion_proposal_privileges_reconcile_test.sql — runnable self-check for ISI-6371.
--
-- Same no-framework discipline as the 0025/0031 companions: plain SQL that fails loudly if the
-- runtime principal loses DML on discussion.proposal — the exact drift that 500'd every
-- proposals endpoint on the long-lived hand-seeded databases. Runs AFTER the full migration set,
-- inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                              -f db/migrations/0041_discussion_proposal_privileges_reconcile_test.sql

BEGIN;

-- (1) Ownership parity: discussion.proposal must be owned by the same principal as its sibling
--     discussion.message — the invariant whose violation 500'd the endpoints (ISI-6371).
DO $$
DECLARE peer_owner text; proposal_owner text;
BEGIN
    SELECT tableowner INTO peer_owner FROM pg_tables
     WHERE schemaname = 'discussion' AND tablename = 'message';
    SELECT tableowner INTO proposal_owner FROM pg_tables
     WHERE schemaname = 'discussion' AND tablename = 'proposal';
    ASSERT proposal_owner = peer_owner,
        format('discussion.proposal owner %s != discussion.message owner %s (ISI-6371: the runtime role cannot DML the table; every proposals endpoint 500s with SQLSTATE 42501)',
               proposal_owner, peer_owner);
END $$;

-- (2) The running principal holds full DML on the table (the capability the incident lost).
DO $$
BEGIN
    ASSERT has_table_privilege(current_user, 'discussion.proposal', 'SELECT, INSERT, UPDATE, DELETE'),
        'current_user lacks SELECT/INSERT/UPDATE/DELETE on discussion.proposal (ISI-6371 drift class)';
END $$;

-- (3) The shipped ListProposals read shape runs as the running principal (join smoke).
DO $$
BEGIN
    PERFORM p.phase
      FROM discussion.proposal p
      JOIN discussion.message m ON m.id = p.message_id
      JOIN discussion.thread  t ON t.id  = m.thread_id
     LIMIT 0;
END $$;

ROLLBACK;
