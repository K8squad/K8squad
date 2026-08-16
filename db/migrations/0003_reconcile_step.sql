-- 0003_reconcile_step.sql — durable reconcile_step + §6.3 reclaim marker + §6.6
-- outbox + §6.4 A2A dispatch marker for the crash-safe Run reconcile machine
-- (Story 3.1 / ISI-2655; the physical integration of pkg/reconcile, ISI-2535).
--
-- Forward-only companion to 0001_coord_schema.sql and 0002_coord_dispatch.sql.
-- The reconcile machine (pkg/reconcile) is proven in the shipping language over a
-- small Store/Effects seam; this migration is the durable substrate that seam's
-- production Store (pkg/coord/prodreconcile.go, coord.ProdReconcileStore) binds
-- to. It adds exactly what the seam reads and CAS-writes — nothing about the
-- state-machine logic itself lives here.
--
-- What each object gives the machine (arch §6.3/§6.4/§6.5/§6.6):
--   * coord.claim.reconcile_step   — the DURABLE fine-grained step (AC2). The
--       Postgres source of truth the failover leader re-enters from; status.phase
--       is a projection of it (reconcile.PhaseOf), never the reverse.
--   * coord.claim.reclaim_fenced_at — the §6.3 fence-first-reclaim marker: stamped
--       when a dead holder's fence is bumped out BEFORE the failover leader
--       re-enters, so the reclaim is observable/auditable, not just a token bump.
--   * coord.outbox                 — the §6.6 transactional outbox. Store.Advance
--       co-commits ONE outbox row with the step-advance in the SAME transaction
--       (AC6), so a downstream relay (CRD status patch, SSE) projects the durable
--       step with exactly-once delivery semantics — no dual-write to K8s that can
--       diverge from Postgres on a crash between the two writes.
--   * coord.a2a_dispatch           — the §6.4 A2A dispatch idempotency marker for
--       the `dispatching` step: a deterministic a2a_task_id (= run_id, per retry
--       lap) so a crash-window re-drive reattaches to the in-flight task instead
--       of starting a second agent execution. Distinct from coord.dispatch (0002),
--       which is the coordinator DELEGATION loop's get-or-create marker.
--
-- The reconcile_step column lives ON coord.claim (not a side table) so the machine's
-- load-bearing conditional advance is a SINGLE guarded UPDATE co-locating the step
-- with the fence that guards it:
--     UPDATE coord.claim SET reconcile_step = :next
--      WHERE work_item_id = :item AND reconcile_step = :expected AND fence_token = :fence
-- The reconcile_step = :expected step-CAS serializes two same-fence leaders to
-- exactly one advance (AC4); the fence_token = :fence equality covers the
-- bumped-out reclaim zombie (AC4). Both guards read one row — no cross-table race.

-- ---------------------------------------------------------------------------
-- coord.claim — durable reconcile_step + §6.3 reclaim marker
-- ---------------------------------------------------------------------------
-- reconcile_step DEFAULTs to 'pending' so every pre-provisioned claim row (the
-- work_item insert trigger already creates one per item, unheld) starts the
-- machine at the head of the happy path with no extra write. The CHECK pins the
-- exact reconcile.Step enum — a code path that tried to persist a step the machine
-- cannot classify (reconcile.ClassUnknown) is rejected at the source, not left to
-- surface as a stuck Run. Keep this list in lockstep with pkg/reconcile/machine.go.
ALTER TABLE coord.claim
    ADD COLUMN reconcile_step    text        NOT NULL DEFAULT 'pending'
        CONSTRAINT claim_reconcile_step_enum CHECK (reconcile_step IN (
            'pending', 'claiming_sandbox', 'dispatching', 'running', 'collecting',
            'succeeded', 'failed', 'cancelled', 'paused', 'paused(rate_limited)')),
    ADD COLUMN reclaim_fenced_at timestamptz;   -- §6.3: NULL until a fence-first reclaim stamps it

-- Failover re-entry scan: "which held claims are mid-flight (non-terminal step)
-- and due for a reconcile pass". Partial index keeps it to the live working set.
CREATE INDEX idx_claim_reconcile_active ON coord.claim (reconcile_step)
    WHERE reconcile_step NOT IN ('succeeded', 'failed', 'cancelled');

-- ---------------------------------------------------------------------------
-- coord.outbox — the §6.6 transactional outbox (co-committed with each advance)
-- ---------------------------------------------------------------------------
-- One row per committed reconcile_step advance, written in the SAME transaction as
-- the coord.claim UPDATE and the §6.5 audit row (AC6). A relay projects unpublished
-- rows onto Run.status (PhaseOf) / SSE and stamps published_at; the durable step in
-- coord.claim remains the source of truth, the outbox is the exactly-once delivery
-- channel FROM it. This is a delivery ledger, NOT an agent-to-agent message table
-- (I4/no-P2P): every field is coordination-record data the audit log already holds.
CREATE TABLE coord.outbox (
    id           bigserial   PRIMARY KEY,   -- monotonic delivery sequence
    work_item_id uuid        NOT NULL REFERENCES coord.work_item(id) ON DELETE RESTRICT,
    run_id       uuid,                        -- the Run whose reconcile advanced (NULL only for the naive model)
    event_type   text        NOT NULL,        -- 'reconcile_advanced' (room for later §6.6 event kinds)
    from_step    text,                         -- provenance: the step advanced FROM
    to_step      text        NOT NULL,         -- the step advanced TO (the projected phase source)
    fence_token  bigint,                       -- fence under which the advance committed
    payload      jsonb,                        -- structured detail for the projector
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz                   -- NULL = not yet relayed to status/SSE
);
-- Relay scan: oldest-first over the unpublished tail only.
CREATE INDEX idx_outbox_unpublished ON coord.outbox (id) WHERE published_at IS NULL;
CREATE INDEX idx_outbox_work_item   ON coord.outbox (work_item_id, id);

-- The outbox is an append+mark ledger: rows are INSERTed and later have
-- published_at set once. DELETE/TRUNCATE are never on the intended path (retention,
-- if ever needed, is a bounded published-tail prune, a later story) — guard against
-- a stray wipe the same way the audit log is guarded (0001 reject-mutation pattern
-- for DELETE/TRUNCATE; UPDATE is allowed because published_at must be settable).
CREATE FUNCTION coord.reject_outbox_wipe() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'coord.outbox is delete-guarded: % rejected (§6.6 delivery ledger)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
CREATE TRIGGER outbox_no_delete
    BEFORE DELETE ON coord.outbox
    FOR EACH ROW EXECUTE FUNCTION coord.reject_outbox_wipe();
CREATE TRIGGER outbox_no_truncate
    BEFORE TRUNCATE ON coord.outbox
    FOR EACH STATEMENT EXECUTE FUNCTION coord.reject_outbox_wipe();

-- ---------------------------------------------------------------------------
-- coord.a2a_dispatch — §6.4 A2A dispatch idempotency marker (dispatching step)
-- ---------------------------------------------------------------------------
-- The `dispatching` step submits an A2A task to the shim. The task id is
-- DETERMINISTIC (a2a_task_id = run_id, or run_id#lapN across §8 retry laps), so a
-- crash between "task submitted" and "reconcile_step advanced" re-drives onto the
-- SAME id: this marker's PK makes the second submit a no-op (INSERT … ON CONFLICT
-- DO NOTHING), and the shim reattaches to the in-flight task instead of starting a
-- second agent execution. Stores NO content and NO delivery state — the durable
-- get-or-create marker only. a2a_task_id is text (the #lapN suffix is not a uuid).
CREATE TABLE coord.a2a_dispatch (
    a2a_task_id   text        PRIMARY KEY,   -- deterministic: run_id, or 'run_id#lapN' per retry lap
    work_item_id  uuid        NOT NULL REFERENCES coord.work_item(id) ON DELETE RESTRICT,
    run_id        uuid        NOT NULL,      -- the Run this dispatch belongs to
    dispatched_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_a2a_dispatch_run ON coord.a2a_dispatch (work_item_id, run_id);
