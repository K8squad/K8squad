-- 0044_run_event_retention.sql — ISI-7484 WS-3 (parent ISI-7482/ISI-7480):
-- governed retention for the durable run-step journal (coord.outbox entity='run').
--
-- WHY: since the ISI-7074 family the run-step journal (thinking / tool /
-- lifecycle / ticket_comment) lives ONLY in the outbox — the SSE projector
-- live-tails it, Last-Event-ID replay resumes it, and the new REST history
-- (GET /api/runs/{id}/events, ISI-7484) hydrates Activity from it. The outbox
-- is therefore a long-lived REPLAY store, not just the NATS relay backlog, and
-- needs a bounded life instead of unbounded growth.
--
-- CONTRACT: 0003 made the outbox append-only — the guard rejects DELETE
-- outright. This migration KEEPS that guarantee for every ordinary writer and
-- adds exactly ONE sanctioned prune path: coord.prune_run_events(cutoff),
-- which may delete ONLY entity='run' rows that are already relayed
-- (published_at IS NOT NULL) and older than the cutoff. Arbitrary DELETEs —
-- any entity, or a run row from ordinary app code — still trip the guard: the
-- amended guard allows a run-row DELETE only while the transaction-local GUC
-- coord.outbox_run_retention='on' is set, and only the SECURITY DEFINER
-- prune function sets it. The NATS relay contract is untouched: the relay
-- scans published_at IS NULL only (idx_outbox_unpublished), and prune never
-- touches an unflushed row, so at-least-once delivery and the set-once
-- published_at marker are preserved. Raw telemetry is NOT re-persisted into
-- coord.comment anywhere here (ISI-7454 guardrail).

-- (1) Guard amendment: DELETE stays rejected except the single governed
--     run-retention path. CREATE OR REPLACE keeps the trigger binding (the
--     trigger references the function by name/OID, which REPLACE preserves),
--     so every existing clause of 0003 continues to apply unchanged.
CREATE OR REPLACE FUNCTION coord.outbox_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- ISI-7484: the one sanctioned retention exception — run-entity rows,
        -- only inside the prune function transaction (its SECURITY DEFINER
        -- body sets the transaction-local GUC below). Every other DELETE —
        -- any entity, any other session, any other code path — still trips
        -- the append-only guard exactly as before.
        IF OLD.entity = 'run'
           AND current_setting('coord.outbox_run_retention', true) = 'on' THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'coord.outbox is append-only: DELETE rejected (§17.4)'
            USING ERRCODE = 'restrict_violation';
    END IF;
    -- Every event column is immutable; only published_at may move.
    IF ( NEW.id, NEW.entity, NEW.project_id, NEW.squad, NEW.event_type,
         NEW.work_item_id, NEW.run_id, NEW.payload, NEW.occurred_at, NEW.trace_carrier )
       IS DISTINCT FROM
       ( OLD.id, OLD.entity, OLD.project_id, OLD.squad, OLD.event_type,
         OLD.work_item_id, OLD.run_id, OLD.payload, OLD.occurred_at, OLD.trace_carrier ) THEN
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

-- (2) The governed prune function — the ONLY writer of the GUC. Deletes relayed
--     run rows older than the cutoff and returns the row count (observability
--     for the apiserver retention sweep). SECURITY DEFINER + REVOKE FROM PUBLIC
--     keeps it callable only by the owning migration/runtime role.
CREATE FUNCTION coord.prune_run_events(p_cutoff timestamptz) RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = coord, pg_temp
AS $$
DECLARE
    v_deleted bigint;
BEGIN
    IF p_cutoff IS NULL THEN
        RAISE EXCEPTION 'prune_run_events: cutoff is required';
    END IF;
    -- Transaction-local: the guard exception is armed for exactly this call,
    -- never for the caller's session after commit/rollback.
    PERFORM set_config('coord.outbox_run_retention', 'on', true);
    DELETE FROM coord.outbox
     WHERE entity = 'run'
       AND published_at IS NOT NULL
       AND occurred_at < p_cutoff;
    GET DIAGNOSTICS v_deleted = ROW_COUNT;
    RETURN v_deleted;
END;
$$;
REVOKE ALL ON FUNCTION coord.prune_run_events(timestamptz) FROM PUBLIC;

-- (3) Prune-scan index: exactly the prune predicate (relayed run rows by age),
--     so the periodic sweep stays a cheap index scan as the journal grows.
CREATE INDEX idx_outbox_run_retention ON coord.outbox (occurred_at)
    WHERE entity = 'run' AND published_at IS NOT NULL;
