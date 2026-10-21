-- 0041_discussion_proposal_privileges_reconcile_test.sql — runnable self-check for ISI-6371.
--
-- Plain-SQL assertions like the other *_test.sql companions; fails loudly if the running
-- principal loses DML on the incident tables — the exact drift that 500'd every proposals
-- endpoint on the long-lived hand-seeded databases. Anchored on CAPABILITY (what the running
-- role can actually do), not on ownership parity alone: parity passes in the both-tables-drifted
-- posture a wrong-direction reconcile could produce, capability does not.
--
-- Runs AFTER the full migration set, inside one transaction ROLLED BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--         -f db/migrations/0041_discussion_proposal_privileges_reconcile.sql \
--         -f db/migrations/0041_discussion_proposal_privileges_reconcile_test.sql
--
-- (one -f per file — a shell glob expands to several operands and psql takes the second
-- filename as the USERNAME; see db/migrations/README.md)

BEGIN;

-- (1) Capability anchor: the running principal holds ALL FOUR DML privileges on the incident
--     table and its sibling. has_table_privilege with a comma list is ANY-of, not ALL-of
--     (a SELECT-only role passes it — measured on Postgres 16), hence four ANDed calls.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['discussion.message', 'discussion.proposal'] LOOP
        ASSERT has_table_privilege(current_user, t, 'SELECT')
           AND has_table_privilege(current_user, t, 'INSERT')
           AND has_table_privilege(current_user, t, 'UPDATE')
           AND has_table_privilege(current_user, t, 'DELETE'),
            format('%s: current_user lacks one of SELECT/INSERT/UPDATE/DELETE (ISI-6371 drift class; every proposals endpoint 500s with SQLSTATE 42501)', t);
    END LOOP;
END $$;

-- (2) Standing shape: within every managed schema, all tables share ONE owner (the runtime DDL
--     role). A hand-applied migration (the ISI-5051 seeding era) breaks this loudly here and in
--     the apiserver's every-boot dbmigrate.Verify, instead of 500-ing in production.
DO $$
DECLARE s text; owners int;
BEGIN
    FOREACH s IN ARRAY ARRAY['auth', 'coord', 'discussion', 'scm'] LOOP
        SELECT count(DISTINCT tableowner) INTO owners FROM pg_tables WHERE schemaname = s;
        ASSERT owners <= 1,
            format('schema %s has %s distinct table owners (ISI-6371 drift class; run db/migrations/0041_discussion_proposal_privileges_reconcile.sql once as a superuser)', s, owners);
    END LOOP;
END $$;

-- (3) The shipped ListProposals read shape runs as the running principal (join smoke; LIMIT 0
--     still permission-checks and raises 42501 on a drifted table).
DO $$
BEGIN
    PERFORM p.phase
      FROM discussion.proposal p
      JOIN discussion.message m ON m.id = p.message_id
      JOIN discussion.thread  t ON t.id  = m.thread_id
     LIMIT 0;
END $$;

ROLLBACK;
