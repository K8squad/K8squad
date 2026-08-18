-- 0007_reconcile_effects.sql — durable warm-pool sandbox-bind idempotency ledger
-- for the production reconcile.Effects seam (Story 3.1 / ISI-2655, child ISI-2802).
--
-- Forward-only companion to 0001_coord_schema.sql and 0005_reconcile_step.sql. The
-- crash-safe Run reconcile machine (pkg/reconcile) drives four side effects through
-- the reconcile.Effects seam; this migration adds the ONE durable substrate that
-- seam's production impl (pkg/coord/prodeffects.go, coord.ProdEffects) still needs.
-- The other three effects already have their durable homes on the checked-in
-- schema and need NO new table:
--   * Dispatch → coord.a2a_dispatch  (0005; a2a_task_id PK is the §6.4 dedup guard)
--   * Collect  → coord.artifact      (0001; UNIQUE(work_item,run,kind) upsert key)
--   * Terminal → coord.audit_log     (0001; the §6.5 provenance record)
--
-- What this adds (arch §6.2/§9, Story 3.2/3.4):
--   * coord.sandbox_bind — the §6.4 warm-pool bind idempotency marker for the
--       `claiming_sandbox` step. Keyed by run_id: a crash between "sandbox
--       provisioned" and "reconcile_step advanced" re-drives onto the SAME run_id,
--       and this marker's PK makes the second bind a no-op (INSERT … ON CONFLICT
--       DO NOTHING) so the warm-pool bind reattaches instead of double-provisioning
--       a second sandbox. It mirrors coord.a2a_dispatch (0005) for the sibling
--       `dispatching` step: one at-most-once marker per side effect, keyed by the
--       deterministic run identifier the machine re-enters on.
--
-- FR-B3: coord.sandbox_bind is NOT an agent-to-agent chat channel — it records only
-- WHICH physical sandbox a Run bound and WHO drove the bind (custody/provenance),
-- never worker-authored content, and nothing stored here re-enters coordination.
-- Custody still moves only via coord.comment + a work_item state change (§6.1).

-- ---------------------------------------------------------------------------
-- coord.sandbox_bind — §6.4 warm-pool bind idempotency marker (claiming_sandbox)
-- ---------------------------------------------------------------------------
-- run_id is the PK (the idempotency key): exactly one sandbox bind per Run, so a
-- re-driven BindSandbox on the same run_id is a structural no-op. sandbox_ref is the
-- opaque physical warm-pool handle stamped on first provision (Story 3.4 supplies
-- the physical binder; DEFAULT '' covers the ledger-only mode that lands ahead of
-- the physical adapter). Stores NO content — the durable get-or-create marker only.
CREATE TABLE coord.sandbox_bind (
    run_id       uuid        PRIMARY KEY,   -- deterministic: the Run this sandbox is bound to
    work_item_id uuid        NOT NULL REFERENCES coord.work_item(id) ON DELETE RESTRICT,
    sandbox_ref  text        NOT NULL DEFAULT '',  -- opaque physical warm-pool handle (Story 3.4)
    bound_by     text        NOT NULL,      -- principal that drove the bind (§6.5 provenance)
    bound_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_sandbox_bind_item ON coord.sandbox_bind (work_item_id);
