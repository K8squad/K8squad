-- 0026_discussion_project_id_text.sql — ISI-4919: the discussion room's project_id is the platform
-- Project id (a Kubernetes "namespace/name" composite, ISI-3982), NOT a uuid.
--
-- WHY: every OTHER project-scoped apiserver surface (dashboard, github, files, issue-links, overview,
-- rbac) resolves {projectId} with decodePathVar as the "namespace/name" slug string. The discussion
-- feature alone parsed {projectId} as a uuid (handler.pathUUID) and stored it in a `uuid` column, so
-- EVERY real room — whose id is a slug like "sympozium-squad/sympozium-todo-demo" — 400'd with
-- "invalid projectId" before any query ran, and the console surfaced "Could not open the discussion
-- room." The room has therefore never opened for any real Project; there is no production data keyed
-- by a real project uuid to preserve (only self-retracted repro rows, which cast cleanly to text).
--
-- FIX: retype discussion.thread.project_id uuid -> text so it holds the canonical "namespace/name"
-- id, matching the rest of the platform. The composite index idx_thread_project_team (project_id,
-- team_id) is rebuilt automatically by the type change; the NOT NULL constraint is preserved.
--
-- Forward-only, additive-in-spirit (a widening retype of an un-joined room-key column); applied once
-- by the apiserver migration runner (ISI-5051) in filename order.

ALTER TABLE discussion.thread
    ALTER COLUMN project_id TYPE text USING project_id::text;
