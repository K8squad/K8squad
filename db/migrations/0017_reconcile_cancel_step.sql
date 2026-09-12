-- 0017_reconcile_cancel_step.sql — the reconcile_step enum extension for the
-- Story 3.3 operator-kill transitional step (ISI-4299).
--
-- Forward-only companion to 0005_reconcile_step.sql and
-- 0010_credential_pause.sql. Story 3.3 (PR #138, ISI-2884) introduced the
-- TWO-TRANSITION kill protocol — CancelEnter (apiserver, fence-first) moves a
-- running-ish claim to the transitional step 'cancelling'; CancelFinish (the
-- operator drive loop) completes cancelling → cancelled after the sandbox
-- teardown — and pkg/reconcile/machine.go has carried StepCancelling since.
-- But NO migration ever admitted the value: 0005 pinned the CHECK through
-- 'paused(rate_limited)' and 0010's replacement (the credential pause family)
-- reproduced the omission, so every CancelEnter on a repo-built DB died with
-- 23514 check constraint "claim_reconcile_step_enum" violation. KillRun has
-- never executed end-to-end against the shipped schema (ISI-4299 defect 1).
--
-- A CHECK cannot be extended in place, so we DROP and re-ADD with the full
-- closed set — the same pattern 0010 used. Keep this list in lockstep with
-- pkg/reconcile/machine.go (Step consts). Forward-only: the added value is a
-- superset, so no existing row can violate the new constraint. Databases where
-- the constraint was extended by hand to unblock the ISI-4220 evidence kill
-- (k8squad-test) converge back onto this canonical definition when the runner
-- applies 0017 — the DROP/re-ADD is shape-agnostic about how the live
-- constraint got there.
--
-- The partial index idx_claim_reconcile_active (0005) keeps 'cancelling' in
-- its active scan set deliberately: a cancelling claim is mid-teardown and due
-- for the operator's §3.3 backstop cancel sweep (ProdCancelStore.Due), so it
-- belongs on the failover re-entry working set, not the terminal set.
ALTER TABLE coord.claim DROP CONSTRAINT claim_reconcile_step_enum;
ALTER TABLE coord.claim
    ADD CONSTRAINT claim_reconcile_step_enum CHECK (reconcile_step IN (
        'pending', 'claiming_sandbox', 'dispatching', 'running', 'collecting',
        'succeeded', 'failed', 'cancelled', 'cancelling',
        'paused', 'paused(rate_limited)',
        'paused(credential_expired)', 'paused(credential_rotated)',
        'paused(endpoint_unreachable)'));
