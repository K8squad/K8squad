-- 0026_discussion_project_id_text_test.sql — runnable self-check for the project_id retype
-- (ISI-4919). Same discipline as the sibling self-checks: plain SQL, no framework, run after the
-- migrations it checks, inside one transaction rolled back at the end so it leaves no residue:
--
--     psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
--          -f db/migrations/0004_discussion_schema.sql \
--          -f db/migrations/0024_discussion_message_fields.sql \
--          -f db/migrations/0025_discussion_proposal.sql \
--          -f db/migrations/0026_discussion_project_id_text.sql \
--          -f db/migrations/0026_discussion_project_id_text_test.sql
--
-- Proves the STRUCTURAL AC: after 0026 the room key is text (not uuid), still NOT NULL, the
-- composite (project_id, team_id) index survives the retype, and a real "namespace/name" Project
-- slug — the id shape that 400'd before this fix — inserts and reads back verbatim.

BEGIN;

-- (1) project_id is now text, and still NOT NULL.
DO $$
BEGIN
    ASSERT (SELECT data_type FROM information_schema.columns
             WHERE table_schema='discussion' AND table_name='thread' AND column_name='project_id') = 'text',
        'discussion.thread.project_id must be text after 0026';
    ASSERT (SELECT is_nullable FROM information_schema.columns
             WHERE table_schema='discussion' AND table_name='thread' AND column_name='project_id') = 'NO',
        'discussion.thread.project_id must remain NOT NULL';
END $$;

-- (2) The composite room+tenancy index survives the retype.
DO $$
BEGIN
    ASSERT to_regclass('discussion.idx_thread_project_team') IS NOT NULL,
        'idx_thread_project_team must survive the project_id retype';
END $$;

-- (3) A canonical "namespace/name" Project slug — the exact shape that used to 400 with
--     "invalid projectId" — now round-trips as the room key.
DO $$
DECLARE tid uuid;
BEGIN
    INSERT INTO discussion.thread (project_id, team_id, title, created_by)
         VALUES ('sympozium-squad/sympozium-todo-demo', gen_random_uuid(), 'General', 'user:test')
      RETURNING id INTO tid;
    ASSERT (SELECT project_id FROM discussion.thread WHERE id = tid) = 'sympozium-squad/sympozium-todo-demo',
        'a namespace/name Project slug must round-trip as project_id';
END $$;

ROLLBACK;
