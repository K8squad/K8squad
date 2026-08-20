-- 0003_discussion_schema.sql — Per-Project Discussion Room schema (Arch §7.5, Story 10.1 / ISI-2702)
--
-- Forward-only, versioned migration (same discipline as 0001_coord_schema.sql). Applied exactly
-- once, in filename order, by the apiserver migration runner on startup. There is NO down migration.
--
-- WHAT THIS IS (ADR-001 / ADR-019): a Postgres *schema*, not a new datastore and NOT a CRD. Each
-- `Project` gets one persistent, threaded, provenance-tagged discussion room the memory service can
-- later index (10.2) and the console can render (10.3). "Conversation, not custody." (§7.5)
--
-- THE LOAD-BEARING INVARIANTS THIS STRUCTURE ENFORCES (not application discipline alone):
--   * The room is the Project (R1)         — NO `discussion_room` table; the room is keyed by
--                                             `thread.project_id`, 1:1 with the Project by construction.
--   * Append-only + soft-retract (§7.4)     — BEFORE UPDATE/DELETE + BEFORE TRUNCATE triggers reject
--                                             any destructive mutation; the ONLY permitted update is the
--                                             one-way `invalidated_at` NULL→timestamp soft-retract.
--   * Server-stamped provenance (§6.5/§7.3.1)— `author_principal`/`created_by`/`created_at` are NOT
--                                             NULL; the *server* fills them from the authenticated
--                                             context (enforced in the write path — §7.3.1 rule).
--   * Tenancy is un-skippable (§7.3.3/NFR-SEC7) — `project_id`, `team_id`, `author_principal`,
--                                             `created_at` are NOT NULL, so an unscoped or unattributed
--                                             row is un-representable.
--   * Structurally coordination-free (§7.5, the fence) — there is NO `claim`/`lease`/`fence_token`/
--                                             `state`/`holder`/`assignee`/status column and NO
--                                             custody-transfer expression. Custody of a work item moves
--                                             ONLY in the fenced `coord` claim tables (§6.2/§6.3).
--                                             (Story 10.4 turns this into a *tested* guarantee, F6.)
--
-- Name mapping (Arch §7.5 wording ↔ durable names — same reconciliation coord did for §6.1):
--   §7.5 lists the tables conceptually as `discussion_thread` / `discussion_message`. The durable
--   names follow the coord precedent (schema-qualified, unstuttered singular nouns, cf.
--   `coord.work_item`): schema `discussion`, tables `discussion.thread` and `discussion.message`.
--     §7.5 `discussion_thread`  ≡  discussion.thread
--     §7.5 `discussion_message` ≡  discussion.message
--   The room ("discussion_room" in the epic prose) is DELIBERATELY not a table (R1).
--
-- SUPERSEDES the naive ISI-2147 shape (`migrations/001_create_discussion_rooms.sql`: a
-- `discussion_rooms` table, client-supplied `author_id`/`author_type`/`author_name`, no `team_id`,
-- `ON DELETE CASCADE`, `edited_at`). That shape violates R1/AC2/AC3/AC5; this migration is the §7.5
-- authority. Do NOT run both.

CREATE SCHEMA IF NOT EXISTS discussion;

-- gen_random_uuid() is a core function since PG13 (CNPG ships ≥13) — no pgcrypto extension needed,
-- so the migration runs under a least-privilege app role without CREATE EXTENSION rights (mirrors 0001).

-- ---------------------------------------------------------------------------
-- discussion.thread — a thread in a Project's room (§7.5)
--
-- The room IS the Project: there is no `discussion_room` table. `project_id` is the room key and is
-- 1:1 with the Project by construction; a Project's room is addressable the instant the Project
-- exists (GET .../discussion/threads returns []), with no provisioning step, finalizer, or seed row.
-- ---------------------------------------------------------------------------
CREATE TABLE discussion.thread (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid        NOT NULL,                    -- room key — tenancy + 1:1-with-Project (R1)
    team_id     uuid        NOT NULL,                    -- tenancy root (§7.3.3 filter; = squad/namespace)
    title       text        NOT NULL,
    created_by  text        NOT NULL,                    -- thread opener principal — SERVER-STAMPED (§6.5)
    created_at  timestamptz NOT NULL DEFAULT now()
);
-- Room + tenancy filter (§7.3.3): every read is scoped by (project_id, team_id).
CREATE INDEX idx_thread_project_team ON discussion.thread (project_id, team_id);

-- ---------------------------------------------------------------------------
-- discussion.message — an append-only, provenance-tagged message (§7.5)
--
-- Provenance triple (identical to memory's §7.2, which 10.2's shared pgvector index depends on):
--   author_principal — the authenticated identity (always present)
--   author_agent_id  — set ⇒ agent-authored; NULL ⇒ human. agent-vs-human is DERIVED, not a flag.
--   author_run_id    — Run linkage (R2); set only when posted from within a Run; NULL otherwise.
-- Threaded via `parent_id` adjacency (like coord.work_item.parent_id), NOT a join table.
-- Soft-retract via `invalidated_at` (§7.4): default reads filter `invalidated_at IS NULL`.
--
-- NOTE the ABSENCE (this is the point — AC4): no claim/lease/fence_token/state/holder/assignee/status
-- column, no custody-transfer expression. This schema CANNOT be a coordination record by construction.
-- ---------------------------------------------------------------------------
CREATE TABLE discussion.message (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id        uuid        NOT NULL REFERENCES discussion.thread(id),           -- owning thread
    parent_id        uuid            NULL REFERENCES discussion.message(id),          -- reply-in-thread (adjacency)
    author_principal text        NOT NULL,                                            -- WHO — SERVER-STAMPED (§7.3.1/§6.5)
    author_agent_id  text            NULL,                                            -- present ⇒ agent; NULL ⇒ human
    author_run_id    text            NULL,                                            -- Run linkage (R2), set only from a Run
    body             text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),                              -- write time (provenance)
    invalidated_at   timestamptz     NULL,                                            -- soft-retract (§7.4); one-way NULL→ts
    CONSTRAINT message_no_self_parent CHECK (parent_id IS NULL OR parent_id <> id)
);
CREATE INDEX idx_message_thread   ON discussion.message (thread_id);                  -- message fetch
CREATE INDEX idx_message_parent   ON discussion.message (parent_id) WHERE parent_id IS NOT NULL;  -- reply subtree
-- Live-read fast path: default reads filter invalidated_at IS NULL.
CREATE INDEX idx_message_thread_live ON discussion.message (thread_id, created_at) WHERE invalidated_at IS NULL;

-- ---------------------------------------------------------------------------
-- Structural append-only guard (§7.4) — NOT application discipline.
--
-- The forward path has NO hard delete and NO edit: retraction is the soft `invalidated_at` stamp.
-- These triggers back the AC's "no DROP/DELETE/TRUNCATE of the record" with a DB-level guard that
-- even a stray TRUNCATE can't evade (mirrors coord.comment / coord.audit_log immutability).
-- ---------------------------------------------------------------------------

-- Thread is immutable once opened (title/opener/time never change in v1); no retract column on thread.
CREATE OR REPLACE FUNCTION discussion.reject_thread_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'discussion.thread is append-only (attempted %) — threads are immutable (§7.4)', TG_OP;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER trg_thread_no_update BEFORE UPDATE OR DELETE ON discussion.thread
    FOR EACH ROW EXECUTE FUNCTION discussion.reject_thread_mutation();
CREATE TRIGGER trg_thread_no_truncate BEFORE TRUNCATE ON discussion.thread
    FOR EACH STATEMENT EXECUTE FUNCTION discussion.reject_thread_mutation();

-- Message: append-only EXCEPT the one-way soft-retract. The ONLY legal UPDATE sets invalidated_at
-- from NULL to a non-NULL timestamp and changes nothing else. Re-validation (ts→NULL), body/author
-- rewrites, and hard DELETE/TRUNCATE are all rejected — impersonation-via-edit and history erasure
-- are un-representable on the forward path (AC2/AC3/§7.4).
CREATE OR REPLACE FUNCTION discussion.enforce_message_append_only() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'discussion.message is append-only — use the invalidated_at soft-retract, not DELETE (§7.4)';
    END IF;
    -- UPDATE: permit ONLY the soft-retract (NULL → timestamp), and only that column.
    IF OLD.invalidated_at IS NOT NULL THEN
        RAISE EXCEPTION 'discussion.message already retracted — invalidated_at is one-way (§7.4)';
    END IF;
    IF NEW.invalidated_at IS NULL THEN
        RAISE EXCEPTION 'discussion.message UPDATE must set invalidated_at (only the soft-retract is allowed, §7.4)';
    END IF;
    IF ROW(NEW.id, NEW.thread_id, NEW.parent_id, NEW.author_principal, NEW.author_agent_id,
           NEW.author_run_id, NEW.body, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.thread_id, OLD.parent_id, OLD.author_principal, OLD.author_agent_id,
           OLD.author_run_id, OLD.body, OLD.created_at) THEN
        RAISE EXCEPTION 'discussion.message is append-only — only invalidated_at may change on UPDATE (§7.4)';
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER trg_message_append_only BEFORE UPDATE OR DELETE ON discussion.message
    FOR EACH ROW EXECUTE FUNCTION discussion.enforce_message_append_only();

CREATE OR REPLACE FUNCTION discussion.reject_message_truncate() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'discussion.message is append-only — TRUNCATE is forbidden (§7.4)';
END $$ LANGUAGE plpgsql;
CREATE TRIGGER trg_message_no_truncate BEFORE TRUNCATE ON discussion.message
    FOR EACH STATEMENT EXECUTE FUNCTION discussion.reject_message_truncate();

-- Structural thread-integrity guard: a reply's parent must live in the SAME thread. PostgreSQL forbids
-- subqueries in CHECK constraints, so (like the naive impl noted, and like coord's parent-tenancy
-- trigger) this is a BEFORE INSERT trigger rather than a CHECK — but enforced at the DB, not only in app.
CREATE OR REPLACE FUNCTION discussion.enforce_parent_same_thread() RETURNS trigger AS $$
DECLARE parent_thread uuid;
BEGIN
    IF NEW.parent_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT thread_id INTO parent_thread FROM discussion.message WHERE id = NEW.parent_id;
    IF parent_thread IS NULL THEN
        RAISE EXCEPTION 'discussion.message.parent_id % does not exist', NEW.parent_id;
    END IF;
    IF parent_thread <> NEW.thread_id THEN
        RAISE EXCEPTION 'discussion.message.parent_id % belongs to a different thread (%), not %',
            NEW.parent_id, parent_thread, NEW.thread_id;
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER trg_message_parent_same_thread BEFORE INSERT ON discussion.message
    FOR EACH ROW EXECUTE FUNCTION discussion.enforce_parent_same_thread();
