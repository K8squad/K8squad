-- 0022_coord_outbox_trace_carrier.sql — carry the run's W3C trace context on each
-- outbox row so the NATS publish/consume hop joins the run trace (ISI-4440,
-- follow-up to ISI-4238 run tracing; Arch §12.1/§17.4, ADR-023).
--
-- WHY. The run's distributed trace already flows into the sandbox as a W3C
-- carrier (TRACEPARENT/TRACESTATE env; pkg/controller/rundrive/dispatch.go,
-- pkg/warmpool). But the eventing hop is invisible: the relay (pkg/events,
-- Story 12.1) publishes each outbox row to NATS as a bare body with NO trace
-- headers, and the coord write paths capture the event WITHOUT stamping the
-- active trace onto the row. So `nats publish`/`nats receive` never becomes a
-- span in `run.start … run.end` — Henrik (2026-09-15): "we still don't see our
-- event (nats) in the trace produced."
--
-- WHAT. One nullable jsonb column holding the W3C carrier map
-- (`{"traceparent":"00-…","tracestate":"…"}`) that telemetry.Inject stamps at
-- CAPTURE time — inside the SAME transaction as the event, so the trace context
-- and the event are captured atomically (never a row with a mismatched trace).
-- The relay reads it back, telemetry.Extract restores the run's span context,
-- and the NATS producer span it starts (plus the consumer span on the other
-- side) shares the run's trace_id → the event lands IN the run trace. A row with
-- no active span at capture stores NULL and the relay falls back to the
-- payload's `trace_id` (WS-D lifecycle rows) or roots a fresh trace — graceful,
-- never fatal (an outbox write must never depend on a live tracer).
--
-- Forward-only, additive, non-breaking: it is jsonb-nullable with no default, so
-- every existing read/write that never names the column is unaffected, and rows
-- written before this migration keep trace_carrier = NULL (relay fallback path).

ALTER TABLE coord.outbox
    ADD COLUMN trace_carrier jsonb;   -- NULL = no captured trace (relay falls back to payload trace_id / fresh trace)

-- The 0003 immutability guard (coord.outbox_guard) pins the event columns as
-- append-only by comparing an explicit tuple; trace_carrier is captured once
-- with the row and must never mutate thereafter (the relay only ever moves
-- published_at). Fold it into the immutable tuple so the guard rejects any
-- content edit to it exactly as it does for payload/occurred_at. Re-create the
-- function in place (CREATE OR REPLACE keeps the existing triggers bound).
CREATE OR REPLACE FUNCTION coord.outbox_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
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
    -- published_at is set-once: NULL → timestamp only.
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'coord.outbox.published_at is set-once (monotonic flush marker, §17.4)'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
