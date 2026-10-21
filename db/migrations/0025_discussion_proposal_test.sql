-- 0025_discussion_proposal_test.sql — companion for the incident table's own migration.
--
-- Added in the ISI-6371 review: 0025 shipped without a *_test.sql companion, which is plausibly
-- part of why its hand-applied ownership drift went unnoticed until the proposals endpoints
-- 500'd in production. The 0041 companion covers the full drift class across managed schemas;
-- this one keeps the table's own capability assertion at its point of creation.
BEGIN;

DO $$
BEGIN
    ASSERT has_table_privilege(current_user, 'discussion.proposal', 'SELECT')
       AND has_table_privilege(current_user, 'discussion.proposal', 'INSERT')
       AND has_table_privilege(current_user, 'discussion.proposal', 'UPDATE')
       AND has_table_privilege(current_user, 'discussion.proposal', 'DELETE'),
        'current_user lacks one of SELECT/INSERT/UPDATE/DELETE on discussion.proposal (ISI-6371 drift class)';
END $$;

ROLLBACK;
