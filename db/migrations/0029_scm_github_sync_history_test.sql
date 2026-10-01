-- 0029_scm_github_sync_history_test.sql — runnable self-check for ISI-5309 (WS-D.2).
--
-- Same no-framework discipline as 0015: plain SQL that fails loudly if the sync-history surface or
-- its append-only contract breaks. Runs AFTER the full migration set, inside one transaction ROLLED
-- BACK at the end:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/migrations/*.sql \
--                                              -f db/migrations/0029_scm_github_sync_history_test.sql

BEGIN;

-- (1) The table exists in the scm schema with the required provenance columns.
DO $$
DECLARE t text;
BEGIN
    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='scm' AND table_name='github_sync_history' AND column_name='principal';
    ASSERT t = 'text', format('expected scm.github_sync_history.principal text, found %s', coalesce(t,'<absent>'));

    SELECT data_type INTO t FROM information_schema.columns
     WHERE table_schema='scm' AND table_name='github_sync_history' AND column_name='record_count';
    ASSERT t = 'integer', format('expected scm.github_sync_history.record_count integer, found %s', coalesce(t,'<absent>'));
END $$;

-- (2) A valid pass row round-trips; kind / count / ref / principal survive.
DO $$
DECLARE k text; c integer; r text; p text;
BEGIN
    INSERT INTO scm.github_sync_history
        (project_namespace, project_name, repo, kind, issue_ref, record_count, principal, detail)
    VALUES ('team-a', 'proj-x', 'https://github.com/acme/widget', 'poll', NULL, 12, 'ksquad-operator',
            '{"issues":5,"prs":7}'::jsonb);

    SELECT kind, record_count, principal INTO k, c, p
      FROM scm.github_sync_history WHERE project_namespace='team-a' AND project_name='proj-x';
    ASSERT k = 'poll', format('kind round-trip mismatch, found %s', k);
    ASSERT c = 12, format('record_count round-trip mismatch, found %s', c);
    ASSERT p = 'ksquad-operator', format('principal round-trip mismatch, found %s', p);

    -- An import pass carries a per-issue ref.
    INSERT INTO scm.github_sync_history
        (project_namespace, project_name, repo, kind, issue_ref, record_count, principal)
    VALUES ('team-a', 'proj-x', 'https://github.com/acme/widget', 'import', 'acme/widget#42', 1, 'scm-bridge');
    SELECT issue_ref INTO r FROM scm.github_sync_history WHERE kind='import';
    ASSERT r = 'acme/widget#42', format('issue_ref round-trip mismatch, found %s', coalesce(r,'<null>'));
END $$;

-- (3) The kind enum rejects anything outside the known set.
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        INSERT INTO scm.github_sync_history (project_namespace, project_name, kind, principal)
        VALUES ('team-a', 'proj-x', 'sneaky_kind', 'ksquad-operator');
    EXCEPTION WHEN check_violation THEN
        ok := true;
    END;
    ASSERT ok, 'scm.github_sync_history.kind accepted a value outside the allowed set';
END $$;

-- (4) The outcome enum rejects anything outside (success, partial, failed).
DO $$
DECLARE ok boolean := false;
BEGIN
    BEGIN
        INSERT INTO scm.github_sync_history (project_namespace, project_name, kind, outcome, principal)
        VALUES ('team-a', 'proj-x', 'poll', 'exploded', 'ksquad-operator');
    EXCEPTION WHEN check_violation THEN
        ok := true;
    END;
    ASSERT ok, 'scm.github_sync_history.outcome accepted a value outside (success, partial, failed)';
END $$;

-- (5) APPEND-ONLY: UPDATE and DELETE are rejected by scm.reject_mutation, and TRUNCATE is closed too.
DO $$
DECLARE hid bigint; upd boolean := false; del boolean := false; trn boolean := false;
BEGIN
    INSERT INTO scm.github_sync_history (project_namespace, project_name, kind, record_count, principal)
    VALUES ('team-b', 'proj-y', 'webhook', 3, 'ksquad-operator')
    RETURNING id INTO hid;

    BEGIN
        UPDATE scm.github_sync_history SET record_count = 99 WHERE id = hid;
    EXCEPTION WHEN restrict_violation THEN
        upd := true;
    END;
    ASSERT upd, 'scm.github_sync_history UPDATE was not rejected — append-only contract broken';

    BEGIN
        DELETE FROM scm.github_sync_history WHERE id = hid;
    EXCEPTION WHEN restrict_violation THEN
        del := true;
    END;
    ASSERT del, 'scm.github_sync_history DELETE was not rejected — append-only contract broken';

    BEGIN
        TRUNCATE scm.github_sync_history;
    EXCEPTION WHEN restrict_violation THEN
        trn := true;
    END;
    ASSERT trn, 'scm.github_sync_history TRUNCATE was not rejected — append-only contract broken';
END $$;

ROLLBACK;
