-- 0041_discussion_proposal_privileges_reconcile.sql — ISI-6371: reconcile table-ownership
-- drift in the managed schemas (auth, coord, scm, discussion) on long-lived, hand-seeded
-- databases.
--
-- THE INCIDENT: on k8squad-test, migration 0025 was applied BY HAND as the postgres superuser
-- during the ISI-5051 ledger seeding (the dbmigrate runner did not exist yet), so
-- discussion.proposal stayed owned by postgres while every runner-applied table is owned by the
-- role in the apiserver's DATABASE_URL (app on k8squad-system, k8squad on the preview DB). The
-- runtime role then held ZERO privileges on the table and every
-- GET/POST /api/projects/{pid}/discussion/threads/{tid}/proposals failed with
-- "permission denied for table proposal (SQLSTATE 42501)" — surfaced verbatim as a 500 by the
-- console BFF (it forwards apiserver status by design; the ISI-6370 console repro). The same
-- hand-seeding era also drifted coord.change_ref on the preview DB, so this reconcile covers
-- EVERY table in EVERY managed schema, not just the incident table.
--
-- DIRECTION (ISI-6371 review): the intended owner of a schema is the MAJORITY owner of that
-- schema's tables (the runner-created majority) — never a single peer table's owner. A drift on
-- the peer side (message drifted, proposal healthy) therefore cannot drag a healthy table to
-- the wrong role; it repairs the drifted table toward the majority like any other. A tie in
-- owner counts is ambiguous and is REPORTED, never repaired.
--
-- This reconcile is deliberately exception-safe: the runner applies it as the runtime role,
-- which CANNOT reassign a postgres-owned table (that right belongs to the owner/superuser). On a
-- fresh database the runner's own role already owns everything it created, the ownership check
-- short-circuits, and this file is a no-op. On a drifted database the runner warns naming the
-- manual step; the one-time repair is to run this file once as a superuser, e.g.:
--
--     kubectl exec -i -n <db-namespace> <db-pod> -- psql -U postgres -d k8squad \
--         < db/migrations/0041_discussion_proposal_privileges_reconcile.sql
--
-- STANDING INVARIANT (ISI-6371 review): the migration ledger makes this file one-shot, so it is
-- NOT the mechanism that watches for future drift. dbmigrate.Verify (db/dbverify.go) asserts the
-- same invariant on EVERY apiserver boot, independent of ledger state, and logs through the Go
-- logger (SQL WARNING notices never reach the driver logs — the apiserver wires the only
-- NoticeHandler). This file exists so the repair itself has a migration on record.
--
-- Forward-only, additive (no schema change on a healthy database); applied once by the apiserver
-- migration runner in filename order.

DO $$
DECLARE
    managed  text[] := ARRAY['auth', 'coord', 'discussion', 'scm'];
    s        text;
    intended text;
    n        bigint;
    tied     boolean;
    r        record;
BEGIN
    FOREACH s IN ARRAY managed LOOP
        -- Intended owner = the schema's majority table owner (runner-created tables), resolved
        -- deterministically (count DESC, then name) so repeated runs are stable.
        SELECT tableowner, count(*) INTO intended, n
          FROM pg_tables WHERE schemaname = s
         GROUP BY tableowner ORDER BY count(*) DESC, tableowner LIMIT 1;
        CONTINUE WHEN intended IS NULL; -- schema absent (partial-schema test lanes)

        SELECT EXISTS (
            SELECT 1 FROM pg_tables WHERE schemaname = s
             GROUP BY tableowner HAVING count(*) = n AND tableowner <> intended
        ) INTO tied;
        IF tied THEN
            RAISE WARNING 'dbmigrate: 0041 schema % has tied table owners (top owner % holds % of % tables) — direction is ambiguous, refusing to reconcile; resolve by hand', s, intended, n, (SELECT count(*) FROM pg_tables WHERE schemaname = s);
            CONTINUE;
        END IF;

        FOR r IN SELECT tablename, tableowner AS cur FROM pg_tables
                  WHERE schemaname = s AND tableowner <> intended LOOP
            BEGIN
                -- Bound the ACCESS EXCLUSIVE lock the repair takes (ISI-6371 review): a blocked
                -- repair degrades to the warning below instead of wedging a boot behind a reader.
                PERFORM set_config('lock_timeout', '3s', true);
                EXECUTE format('ALTER TABLE %I.%I OWNER TO %I', s, r.tablename, intended);
                RAISE WARNING 'dbmigrate: 0041 reconciled %.% ownership % -> % (ISI-6371 drift class)', s, r.tablename, r.cur, intended;
            EXCEPTION WHEN OTHERS THEN
                -- Deliberately broad (ISI-6371 review: narrowing to insufficient_privilege let
                -- any OTHER failure — e.g. a lock timeout — escape and fail the boot). Every
                -- failure here degrades to a warning carrying the runbook; Apply stays
                -- fail-closed for real migration errors.
                RAISE WARNING 'dbmigrate: 0041 cannot reconcile %.% (owner %) to %: %. Manual repair: run this file once as a superuser (see header); until then, requests touching the table fail with SQLSTATE 42501 (ISI-6371).', s, r.tablename, r.cur, intended, SQLERRM;
            END;
        END LOOP;
    END LOOP;
END $$;
