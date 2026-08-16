-- 0003_coord_outbox.sql — transactional domain-event outbox for the plugin event
-- seam (Arch §6.6/§17.4, ADR-023; Story 12.1 / ISI-2260, FR-B*/FR-E*).
--
-- Forward-only companion to 0001_coord_schema.sql (0001 already REFERENCES this
-- table in prose — "history lives in audit_log / the outbox" — but never created
-- it; this migration is the missing durability substrate). Applied exactly once,
-- in filename order, by the apiserver migration runner on startup.
--
-- WHAT THIS IS (the CEO 2026-08-11 decision, "data in Postgres, events on NATS",
-- ADR-023 r13). Every state change (Run lifecycle §8, work-item transitions §6.6,
-- artifact registration §6.1, memory writes §7.3, scm/CI sync §5.4) writes an
-- append-only event row HERE **in the same transaction** as the state change. The
-- row and the state commit atomically, so an event is captured IFF its state
-- change committed — no lost events, no phantom events, no dual-write hole. A
-- decoupled relay worker (pkg/events, Story 12.1) then publishes each row to a
-- NATS JetStream subject `ksquad.{entity}.{project}.{squad}.{event_type}` and
-- stamps published_at. On restart or NATS-down it republishes unflushed rows →
-- at-least-once even if NATS is unavailable (the outbox IS the durable retry
-- buffer). NATS-down never blocks a Run/claim/write — the write path only INSERTs
-- here; the relay lives outside the coordination transaction (§17.4).
--
-- WHAT THIS IS NOT: not a coordination surface. The seam is emit-only and
-- read-only downstream — no claim/lease/fence column, nothing a plugin publishes
-- re-enters coordination (§17.4 guard, no-P2P). Custody stays in coord.claim.
-- Distinct from coord.dispatch (0002): dispatch is the get-or-create marker for
-- the coordinator loop; this is the event journal for external observers.

-- ---------------------------------------------------------------------------
-- outbox — append-only domain-event journal; published_at is the ONLY mutable
-- column (the relay's set-once flush marker, §17.4).
-- ---------------------------------------------------------------------------
-- bigserial id gives the relay a monotonic scan order (mirrors audit_log). The
-- four subject-taxonomy components (entity/project/squad/event_type) are stored
-- as columns so the relay composes the NATS subject WITHOUT parsing payload.
CREATE TABLE coord.outbox (
    id           bigserial   PRIMARY KEY,                 -- monotonic relay scan order
    entity       text        NOT NULL                     -- §17.4 subject taxonomy entity...
                             CHECK (entity IN ('run','work_item','artifact','memory','scm')),
    -- ...bounded for teeth: a typo'd entity would publish to a garbage NATS subject
    -- and silently mis-deliver. New event sources extend this set via an additive
    -- migration (versioned event catalog discipline, §10.2) — never ad-hoc strings.
    project_id   uuid        NOT NULL,                     -- subject component + tenancy predicate (§12.1)
    squad        text,                                     -- subject component; NULL ⇒ relay uses '_' (work_item.team_id is nullable, §6.1)
    event_type   text        NOT NULL,                     -- e.g. completed|claimed|handoff|paused|artifact_registered|check_run.failed (catalog-governed, §10.2)
    work_item_id uuid        REFERENCES coord.work_item(id) ON DELETE RESTRICT,  -- nullable: Run/scm events need not tie to an item
    run_id       uuid,                                     -- correlation; nullable
    payload      jsonb       NOT NULL,                     -- versioned event body (pkg/events@rev, §10.2)
    occurred_at  timestamptz NOT NULL DEFAULT now(),       -- when the state change committed
    published_at timestamptz                               -- NULL until the relay flushes to NATS (at-least-once marker)
);

-- The relay's hot path: scan the unflushed backlog in id order. Partial index keeps
-- it cheap as published rows accumulate (the vast majority carry published_at).
CREATE INDEX idx_outbox_unpublished ON coord.outbox (id) WHERE published_at IS NULL;

-- ---------------------------------------------------------------------------
-- Immutability guard (§17.4: no lost/phantom/tampered events) — column-scoped, so
-- the blanket coord.reject_mutation() (0001) would be WRONG here: the relay MUST
-- update published_at. This guard rejects DELETE and every content mutation, but
-- allows the one legal write — published_at NULL → timestamp, set once, forward.
-- ---------------------------------------------------------------------------
CREATE FUNCTION coord.outbox_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'coord.outbox is append-only: DELETE rejected (§17.4)'
            USING ERRCODE = 'restrict_violation';
    END IF;
    -- Every event column is immutable; only published_at may move.
    IF ( NEW.id, NEW.entity, NEW.project_id, NEW.squad, NEW.event_type,
         NEW.work_item_id, NEW.run_id, NEW.payload, NEW.occurred_at )
       IS DISTINCT FROM
       ( OLD.id, OLD.entity, OLD.project_id, OLD.squad, OLD.event_type,
         OLD.work_item_id, OLD.run_id, OLD.payload, OLD.occurred_at ) THEN
        RAISE EXCEPTION 'coord.outbox event columns are immutable: only published_at may be set (§17.4)'
            USING ERRCODE = 'restrict_violation';
    END IF;
    -- published_at is set-once: NULL → timestamp only. A re-flush must not rewrite it
    -- (idempotent relay: republishing a row already marked is a no-op, not an UPDATE).
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'coord.outbox.published_at is set-once (monotonic flush marker, §17.4)'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER outbox_immutable_content
    BEFORE UPDATE OR DELETE ON coord.outbox
    FOR EACH ROW EXECUTE FUNCTION coord.outbox_guard();
-- Row triggers do NOT fire on TRUNCATE — reuse the 0001 statement-level guard (F2).
CREATE TRIGGER outbox_no_truncate
    BEFORE TRUNCATE ON coord.outbox
    FOR EACH STATEMENT EXECUTE FUNCTION coord.reject_mutation();

-- ---------------------------------------------------------------------------
-- Wake the relay on commit (§17.4: "LISTEN/NOTIFY + polling"). pg_notify is
-- delivered at COMMIT, so a rolled-back INSERT never wakes the relay for a
-- phantom event. The polling backlog scan (idx_outbox_unpublished) is the
-- durable fallback; NOTIFY only removes latency, it is not relied upon for
-- delivery (a missed NOTIFY is still caught by the next poll → at-least-once).
-- ---------------------------------------------------------------------------
CREATE FUNCTION coord.outbox_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('coord_outbox', NEW.id::text);
    RETURN NEW;
END;
$$;
CREATE TRIGGER outbox_notify_relay
    AFTER INSERT ON coord.outbox
    FOR EACH ROW EXECUTE FUNCTION coord.outbox_notify();
