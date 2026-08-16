-- 0002_coord_dispatch.sql — production §6.4 dispatch marker for the coordinator
-- dispatch loop (Story 2.9 / ISI-2526, FR-B3).
--
-- Forward-only companion to 0001_coord_schema.sql. coord.go's production-binding
-- note called this out: binding the (chaos-proven) §6.4 DispatchOnce semantics to
-- production "requires adding the §6.4 dispatch marker to a forward migration" —
-- this is that migration, shaped for the Story 2.9 delegation-with-feedback loop.
--
-- The coordinator dispatch is IDEMPOTENT BY STRUCTURE: one next work item may be
-- created per (completed source work item, completing run). The dedupe key is
-- deterministic and derived only from coordination-record identifiers, so a
-- reconciler re-drive, a heartbeat retry or M concurrent coordinators all
-- converge on the SAME created item (INSERT … ON CONFLICT (dedupe_key) DO
-- NOTHING + read-back — first-writer-wins, exactly the harness C6 discipline).
--
-- This table stores NO content and NO delivery state: it is the durable
-- get-or-create marker, not an outbox. Title/body/priority live on the created
-- coord.work_item row; provenance lives in coord.audit_log. There is still NO
-- agent-to-agent channel — the marker records only who dispatched what after
-- which completion, every field of which is already coordination-record data.

CREATE TABLE coord.dispatch (
    dedupe_key            text        PRIMARY KEY,   -- deterministic: '<source item>:<source run>:next'
    source_work_item_id   uuid        NOT NULL REFERENCES coord.work_item(id) ON DELETE RESTRICT,
    source_run_id         uuid        NOT NULL,      -- the completing Run whose handoff fed the decision
    created_work_item_id  uuid        NOT NULL REFERENCES coord.work_item(id) ON DELETE RESTRICT,
    coordinator_principal text        NOT NULL,      -- who DECIDED (§6.5 provenance; role check is admission's)
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_dispatch_source ON coord.dispatch (source_work_item_id, source_run_id);
