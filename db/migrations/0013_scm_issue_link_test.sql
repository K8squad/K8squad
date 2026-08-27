-- 0013_scm_issue_link_test.sql — runnable self-check for the story-11.2 issue link table
-- (§5.4/FR-H1, ISI-2738). Same discipline as 0008_scm_mirror_test.sql: plain SQL that
-- fails loudly if the link invariants break, run against a throwaway Postgres AFTER
-- 0013_scm_issue_link.sql is applied, inside one transaction that is ROLLED BACK at
-- the end so the check leaves no residue. Any failed assertion aborts non-zero
-- (ON_ERROR_STOP).

BEGIN;

-- The table exists in the scm schema with the expected column set.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM information_schema.tables
     WHERE table_schema = 'scm' AND table_name = 'issue_link';
    ASSERT n = 1, format('expected scm.issue_link, found %s', n);
END $$;

-- Provenance is pinned to exactly the two origin facts (AC2): ksquad-native vs
-- external-sourced. A provenance-less or freetext link is a schema violation.
DO $$
BEGIN
    BEGIN
        INSERT INTO scm.issue_link
            (project_name, project_namespace, work_item_id, provider, repo, external_id, provenance)
        VALUES ('p','ns', gen_random_uuid(), 'github', 'acme/app', '42', 'unknown-origin');
        RAISE EXCEPTION 'provenance unknown-origin was accepted - freetext provenance breaks the console badge contract';
    EXCEPTION WHEN check_violation THEN
        NULL;  -- expected: the schema itself refuses non-enum provenance
    END;
END $$;

-- Direction is pinned to inbound|bidirectional (AC1 "per configured direction").
DO $$
BEGIN
    BEGIN
        INSERT INTO scm.issue_link
            (project_name, project_namespace, work_item_id, provider, repo, external_id, provenance, direction)
        VALUES ('p','ns', gen_random_uuid(), 'github', 'acme/app', '42', 'ksquad-native', 'outbound-only');
        RAISE EXCEPTION 'direction outbound-only was accepted - an unconfigured direction must not be expressible';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
END $$;

-- The linkage is a bijection: the same external issue cannot be linked twice, and a
-- work item cannot carry two links. Both unique constraints must bite.
INSERT INTO coord.work_item (project_id, title, state, created_by)
VALUES (gen_random_uuid(), 'link self-check item', 'todo', 'self-check');

INSERT INTO scm.issue_link
    (project_name, project_namespace, work_item_id, provider, repo, external_id, provenance)
SELECT 'p','ns', id, 'github', 'acme/app', '42', 'external-sourced' FROM coord.work_item
 WHERE title = 'link self-check item';

DO $$
DECLARE wid uuid;
BEGIN
    SELECT id INTO wid FROM coord.work_item WHERE title = 'link self-check item';
    BEGIN
        INSERT INTO scm.issue_link
            (project_name, project_namespace, work_item_id, provider, repo, external_id, provenance)
        VALUES ('p','ns', wid, 'github', 'acme/app', '43', 'ksquad-native');
        RAISE EXCEPTION 'a second link for the same work item was accepted - linkage must be a bijection';
    EXCEPTION WHEN unique_violation THEN
        NULL;  -- expected: issue_link_unique_work_item
    END;
    BEGIN
        INSERT INTO scm.issue_link
            (project_name, project_namespace, work_item_id, provider, repo, external_id, provenance)
        VALUES ('p','ns', gen_random_uuid(), 'github', 'acme/app', '42', 'ksquad-native');
        RAISE EXCEPTION 'a second link for the same external issue was accepted - linkage must be a bijection';
    EXCEPTION WHEN unique_violation THEN
        NULL;  -- expected: issue_link_unique_external
    END;
END $$;

-- Work-item delete cascades the link away (the external issue simply stops being
-- linked; nothing dangles).
DELETE FROM coord.work_item WHERE title = 'link self-check item';
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM scm.issue_link WHERE provider = 'github' AND repo = 'acme/app';
    ASSERT n = 0, format('work_item delete left %s dangling link(s) - ON DELETE CASCADE broken', n);
END $$;

ROLLBACK;
