-- 0041_discussion_proposal_privileges_reconcile.sql — ISI-6371: reconcile
-- discussion.proposal ownership with the runtime DDL role on long-lived, hand-seeded databases.
--
-- THE INCIDENT: on k8squad-test, migration 0025 was applied BY HAND as the postgres superuser
-- during the ISI-5051 ledger seeding (the dbmigrate runner did not exist yet), so
-- discussion.proposal stayed owned by postgres while every runner-applied table is owned by the
-- role in the apiserver's DATABASE_URL (app on k8squad-system, k8squad on the preview DB). The
-- runtime role then held ZERO privileges on the table and every
-- GET/POST /api/projects/{pid}/discussion/threads/{tid}/proposals failed with
-- "permission denied for table proposal (SQLSTATE 42501)" — surfaced verbatim as a 500 by the
-- console BFF (it forwards apiserver status by design; the ISI-6370 console repro).
--
-- This reconcile is deliberately exception-safe: the runner applies it as the runtime role, which
-- CANNOT reassign a postgres-owned table (that right belongs to the owner/superuser). On a fresh
-- database the runner's own role already owns everything it created, the ownership check
-- short-circuits, and this file is a no-op. On a drifted database the runner logs a loud WARNING
-- naming the manual step; the one-time repair is to run this file once as a superuser, e.g.:
--
--     kubectl exec -i -n <db-namespace> <db-pod> -- psql -U postgres -d k8squad \
--         < db/migrations/0041_discussion_proposal_privileges_reconcile.sql
--
-- (Both long-lived k8squad-test databases were repaired this way when ISI-6371 was root-caused;
-- this file exists so the invariant is asserted — and self-heals wherever privileged — on every
-- roll, and so any future hand-applied migration that lands the same way has a reconciliation
-- on record instead of a silent 500.)
--
-- Forward-only, additive (no schema change on a healthy database); applied once by the apiserver
-- migration runner in filename order.

DO $$
DECLARE
    peer_owner     text;
    proposal_owner text;
BEGIN
    -- The runtime DDL role is whatever owns the rest of the discussion schema (the runner-created
    -- tables); deriving it keeps this file correct on every cluster regardless of role naming.
    SELECT tableowner INTO peer_owner
      FROM pg_tables WHERE schemaname = 'discussion' AND tablename = 'message';
    SELECT tableowner INTO proposal_owner
      FROM pg_tables WHERE schemaname = 'discussion' AND tablename = 'proposal';

    IF peer_owner IS NULL OR proposal_owner IS NULL THEN
        -- Guard the partial-schema lanes (the proposal integration tests apply a subset chain).
        RETURN;
    END IF;
    IF proposal_owner = peer_owner THEN
        RETURN; -- healthy: the runtime role owns the schema it migrated (fresh databases).
    END IF;

    BEGIN
        EXECUTE format('ALTER TABLE discussion.proposal OWNER TO %I', peer_owner);
        RAISE WARNING 'dbmigrate: 0041 reconciled discussion.proposal ownership % -> %',
            proposal_owner, peer_owner;
    EXCEPTION WHEN insufficient_privilege THEN
        RAISE WARNING 'dbmigrate: 0041 cannot reconcile discussion.proposal (owner %) to the runtime owner % — this connection lacks the right. Run db/migrations/0041_discussion_proposal_privileges_reconcile.sql once as a superuser; until then every proposals endpoint fails with SQLSTATE 42501 (ISI-6371)',
            proposal_owner, peer_owner;
    END;
END $$;
