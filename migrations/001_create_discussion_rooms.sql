-- Discussion Rooms: per-project persistent discussion space
-- ADR-001: Postgres storage for threaded, provenance-tagged messages
-- ISI-2147

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- =============================================================================
-- discussion_rooms
-- =============================================================================
CREATE TABLE IF NOT EXISTS discussion_rooms (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id  UUID NOT NULL,
    name        TEXT NOT NULL DEFAULT 'general',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,

    CONSTRAINT uq_room_project_name UNIQUE (project_id, name)
);

CREATE INDEX IF NOT EXISTS idx_rooms_project ON discussion_rooms (project_id) WHERE archived_at IS NULL;

-- =============================================================================
-- discussion_messages
-- =============================================================================
CREATE TABLE IF NOT EXISTS discussion_messages (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    room_id     UUID NOT NULL REFERENCES discussion_rooms(id) ON DELETE CASCADE,
    parent_id   UUID REFERENCES discussion_messages(id) ON DELETE CASCADE,
    author_id   UUID NOT NULL,
    author_type TEXT NOT NULL CHECK (author_type IN ('agent', 'human', 'system')),
    author_name TEXT NOT NULL,
    body        TEXT NOT NULL,
    kind        TEXT NOT NULL DEFAULT 'message' CHECK (kind IN ('message', 'announcement', 'decision', 'question')),
    metadata    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    edited_at   TIMESTAMPTZ

    -- NOTE: the "parent message belongs to the same room" invariant is enforced
    -- in application code (Store.PostMessage validates parent_id's room_id before
    -- insert). It cannot be a CHECK constraint — PostgreSQL forbids subqueries in
    -- CHECK expressions ("cannot use subquery in check constraint"). A DB-level
    -- guard would require a trigger; deferred as out of scope for ISI-2147.
);

CREATE INDEX IF NOT EXISTS idx_messages_room_created ON discussion_messages (room_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_parent ON discussion_messages (parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_messages_author ON discussion_messages (author_id, author_type);
CREATE INDEX IF NOT EXISTS idx_messages_kind ON discussion_messages (room_id, kind);
CREATE INDEX IF NOT EXISTS idx_messages_body_search ON discussion_messages USING gin (to_tsvector('english', body));
CREATE INDEX IF NOT EXISTS idx_messages_metadata ON discussion_messages USING gin (metadata);

-- =============================================================================
-- updated_at trigger for rooms
-- =============================================================================
CREATE OR REPLACE FUNCTION trg_room_touch() RETURNS TRIGGER AS $$
BEGIN
    UPDATE discussion_rooms SET updated_at = now() WHERE id = NEW.room_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Guarded so re-applying this migration is idempotent (CREATE TRIGGER is not
-- IF-NOT-EXISTS-able before PG14; DROP+CREATE works on all supported versions).
DROP TRIGGER IF EXISTS trg_discussion_message_touch ON discussion_messages;
CREATE TRIGGER trg_discussion_message_touch
    AFTER INSERT ON discussion_messages
    FOR EACH ROW
    EXECUTE FUNCTION trg_room_touch();
